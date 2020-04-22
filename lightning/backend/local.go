// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package backend

import (
	"context"
	"path"
	"sync"
	"time"

	"github.com/pingcap/errors"
	sst "github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	pd "github.com/pingcap/pd/v4/client"
	"github.com/pingcap/tidb/table"
	uuid "github.com/satori/go.uuid"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	dbutil "github.com/syndtr/goleveldb/leveldb/util"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	split "github.com/pingcap/br/pkg/restore"

	"github.com/pingcap/tidb-lightning/lightning/common"
	"github.com/pingcap/tidb-lightning/lightning/log"
)

// Range record start and end key for localFile.DB
// so we can write it to tikv in streaming
type Range struct {
	start  []byte
	end    []byte
	length int
}

type localFile struct {
	ts        uint64
	db        *leveldb.DB
	meta      sst.SSTMeta
	length    int64
	totalSize int64

	ranges   []Range
	startKey []byte
	endKey   []byte
}

func (e *localFile) Close() error {
	return e.db.Close()
}

type grpcClis struct {
	mu   sync.Mutex
	clis map[uint64]sst.ImportSSTClient
}

type local struct {
	mu sync.Mutex

	engines  map[uuid.UUID]localFile
	grpcClis *grpcClis
	splitCli split.SplitClient

	filePath        string
	regionSplitSize int64

	mutationPool sync.Pool
}

// NewLocal creates new connections to tikv.
func NewLocalBackend(ctx context.Context, tls *common.TLS, pdAddr string, regionSplitSize int64, filePath string) (Backend, error) {
	pdCli, err := pd.NewClient([]string{pdAddr}, tls.ToPDSecurityOption())
	if err != nil {
		return MakeBackend(nil), errors.Annotate(err, "construct pd client failed")
	}
	allStores, err := pdCli.GetAllStores(ctx, pd.WithExcludeTombstone())
	if err != nil {
		return MakeBackend(nil), errors.Annotate(err, "get all stores failed")
	}
	clients := &grpcClis{
		clis: make(map[uint64]sst.ImportSSTClient),
	}

	tlsConf := tls.TransToTlsConfig()
	if err != nil {
		return MakeBackend(nil), err
	}
	splitCli := split.NewSplitClient(pdCli, tlsConf)

	for _, store := range allStores {
		// create new connection for every store
		conn, err := grpc.DialContext(ctx, store.GetAddress(), tls.ToGRPCDialOption())
		if err != nil {
			return MakeBackend(nil), errors.Annotatef(err, "connect to store failed: %s", store.Address)
		}
		clients.clis[store.GetId()] = sst.NewImportSSTClient(conn)
	}
	return MakeBackend(&local{
		grpcClis: clients,
		engines:  make(map[uuid.UUID]localFile),
		splitCli: splitCli,

		filePath:        filePath,
		regionSplitSize: regionSplitSize,

		mutationPool: sync.Pool{New: func() interface{} { return &sst.Mutation{} }},
	}), nil
}

// Close the importer connection.
func (local *local) Close() {
	local.mu.Lock()
	defer local.mu.Unlock()
	for _, e := range local.engines {
		e.Close()
	}
}

func (local *local) RetryImportDelay() time.Duration {
	return defaultRetryBackoffTime
}

func (local *local) MaxChunkSize() int {
	// 96MB
	return int(local.regionSplitSize)
}

func (local *local) ShouldPostProcess() bool {
	return true
}

func (local *local) OpenEngine(ctx context.Context, engineUUID uuid.UUID) error {
	dbPath := path.Join(local.filePath, engineUUID.String())
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		return err
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	local.engines[engineUUID] = localFile{db: db, length: 0, ranges: make([]Range, 0)}
	return nil
}

func (local *local) CloseEngine(ctx context.Context, engineUUID uuid.UUID) error {
	// Do nothing since we will do prepare jobs in importEngine, just like tikv-importer
	return nil
}

func (local *local) getImportClient(peer *metapb.Peer) (sst.ImportSSTClient, error) {
	local.grpcClis.mu.Lock()
	defer local.grpcClis.mu.Unlock()
	cli, ok := local.grpcClis.clis[peer.GetStoreId()]
	if !ok {
		return nil, errors.Errorf("could not find grpc client for peer id %d", peer.GetId())
	}
	return cli, nil
}

func (local *local) WriteToTiKV(
	ctx context.Context,
	meta *sst.SSTMeta,
	ts uint64,
	region *split.RegionInfo,
	mutations []*sst.Mutation) (metas []*sst.SSTMeta, err error) {
	leader := region.Leader
	if leader == nil {
		leader = region.Region.GetPeers()[0]
	}

	cli, err := local.getImportClient(leader)
	if err != nil {
		return
	}

	wstream, err := cli.Write(ctx)
	if err != nil {
		return
	}

	// Bind uuid for this write request
	req := &sst.WriteRequest{
		Chunk: &sst.WriteRequest_Meta{
			Meta: meta,
		},
	}
	if err = wstream.Send(req); err != nil {
		return
	}

	req.Reset()
	req.Chunk = &sst.WriteRequest_Batch{
		Batch: &sst.WriteBatch{
			CommitTs:  ts,
			Mutations: mutations,
		},
	}
	err = wstream.Send(req)
	if err != nil {
		return
	}

	if resp, closeErr := wstream.CloseAndRecv(); closeErr != nil {
		if err == nil {
			err = closeErr
		}
	} else {
		metas = resp.Metas
		log.L().Debug("get metas after write kv stream to tikv", zap.Reflect("metas", metas))
	}
	return
}

func (local *local) Ingest(ctx context.Context, meta *sst.SSTMeta, region *split.RegionInfo) (*sst.IngestResponse, error) {
	leader := region.Leader
	if leader == nil {
		leader = region.Region.GetPeers()[0]
	}

	cli, err := local.getImportClient(leader)
	if err != nil {
		return nil, err
	}
	reqCtx := &kvrpcpb.Context{
		RegionId:    region.Region.GetId(),
		RegionEpoch: region.Region.GetRegionEpoch(),
		Peer:        leader,
	}

	req := &sst.IngestRequest{
		Context: reqCtx,
		Sst:     meta,
	}
	resp, err := cli.Ingest(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (local *local) ReadAndSplitIntoRange(engineFile localFile) ([]Range, error) {
	if engineFile.length == 0 {
		return nil, nil
	}
	ranges := make([]Range, 0)
	iter := engineFile.db.NewIterator(nil, nil)
	size := int64(0)
	length := 0
	var k, v []byte
	var startKey, endKey []byte
	first := true
	for iter.Next() {
		k = iter.Key()
		v = iter.Value()
		length++
		if first {
			first = false
			startKey = append([]byte{}, k...)
		}
		size += int64(len(k) + len(v))
		if size > local.regionSplitSize {
			endKey = append([]byte{}, k...)
			ranges = append(ranges, Range{start: startKey, end: endKey, length: length})
			first = true
			size = 0
			length = 0
		}
	}
	iter.Release()
	if size > 0 {
		ranges = append(ranges, Range{start: startKey, end: k, length: length})
	}
	return ranges, nil
}

func (local *local) writeAndIngestByRange(
	ctx context.Context,
	iter iterator.Iterator,
	ts uint64,
	length int) error {

	defer iter.Release()
	index := 0
	mutations := make([]*sst.Mutation, length)

	for iter.Next() {
		k := iter.Key()
		v := iter.Value()
		mutations[index] = local.mutationPool.Get().(*sst.Mutation)
		mutations[index] = &sst.Mutation{
			Key:   append([]byte{}, k...),
			Value: append([]byte{}, v...),
			Op:    sst.Mutation_Put,
		}
		index += 1
	}
	if index == 0 {
		return nil
	}

	startKey := mutations[0].Key
	endKey := mutations[index-1].Key
	region, err := local.splitCli.GetRegion(ctx, startKey)
	if err != nil {
		log.L().Error("get region in write failed", zap.Error(err))
		return err
	}

	log.L().Debug("get region",
		zap.Uint64("id", region.Region.GetId()),
		zap.Stringer("epoch", region.Region.GetRegionEpoch()),
		zap.Binary("start", region.Region.GetStartKey()),
		zap.Binary("end", region.Region.GetEndKey()),
		zap.Reflect("peers", region.Region.GetPeers()),
	)

	// generate new uuid for concurrent write to tikv
	meta := &sst.SSTMeta{
		Uuid:        uuid.NewV4().Bytes(),
		RegionId:    region.Region.GetId(),
		RegionEpoch: region.Region.GetRegionEpoch(),
		Range: &sst.Range{
			Start: startKey,
			End:   endKey,
		},
	}
	metas, err := local.WriteToTiKV(ctx, meta, ts, region, mutations)
	if err != nil {
		log.L().Error("write to tikv failed", zap.Error(err))
		return err
	}

	for _, mutation := range mutations {
		local.mutationPool.Put(mutation)
	}

	for i := 0; i < maxRetryTimes; i ++ {
		for _, meta := range metas {
			resp, err := local.Ingest(ctx, meta, region)
			if err != nil {
				log.L().Error("ingest to tikv failed", zap.Error(err))
				return err
			}
			needRetry, newRegion, errIngest := isIngestRetryable(resp, region, meta)
			if !needRetry {
				return errIngest
			}
			// retry with not leader and epoch not match error
			region = newRegion
		}
	}
	return nil
}

func (local *local) WriteAndIngestByRanges(ctx context.Context, engineFile localFile, ranges []Range) error {
	if engineFile.length == 0 {
		return nil
	}
	var eg errgroup.Group
	for _, r := range ranges {
		log.L().Debug("deliver range",
			zap.Binary("start", r.start),
			zap.Binary("end", r.end),
			zap.Int("len", r.length))
		iter := engineFile.db.NewIterator(&dbutil.Range{Start: r.start, Limit: nextKey(r.end)}, nil)
		length := r.length
		eg.Go(func() error {
			return local.writeAndIngestByRange(ctx, iter, engineFile.ts, length)
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

func (local *local) ImportEngine(ctx context.Context, engineUUID uuid.UUID) error {
	local.mu.Lock()
	engineFile, ok := local.engines[engineUUID]
	defer local.mu.Unlock()
	if !ok {
		return errors.Errorf("could not find engine %s in ImportEngine", engineUUID.String())
	}
	// split sorted file into range by 96MB size per file
	ranges, err := local.ReadAndSplitIntoRange(engineFile)
	if err != nil {
		return err
	}
	// split region by given ranges
	err = local.SplitAndScatterRegionByRanges(ctx, ranges)
	if err != nil {
		log.L().Error("split & scatter ranges failed", zap.Error(err))
		return err
	}
	// start to write to kv and ingest
	err = local.WriteAndIngestByRanges(ctx, engineFile, ranges)
	if err != nil {
		log.L().Error("write and ingest ranges failed", zap.Error(err))
		return err
	}
	return nil
}

func (local *local) CleanupEngine(ctx context.Context, engineUUID uuid.UUID) error {
	// release this engine after import success
	local.mu.Lock()
	defer local.mu.Unlock()
	engineFile := local.engines[engineUUID]
	engineFile.Close()
	delete(local.engines, engineUUID)
	return nil
}

func (local *local) WriteRows(
	ctx context.Context,
	engineUUID uuid.UUID,
	tableName string,
	columnNames []string,
	ts uint64,
	rows Rows,
) (finalErr error) {
	kvs := rows.(kvPairs)
	if len(kvs) == 0 {
		return nil
	}

	local.mu.Lock()
	engineFile, ok := local.engines[engineUUID]
	local.mu.Unlock()
	if !ok {
		return errors.Errorf("could not find engine for %s", engineUUID.String())
	}

	// write to go leveldb get get sorted kv
	batch := new(leveldb.Batch)
	size := int64(0)
	for _, pair := range kvs {
		batch.Put(pair.Key, pair.Val)
		size += int64(len(pair.Key) + len(pair.Val))
	}
	engineFile.length += int64(batch.Len())
	engineFile.totalSize += size
	err := engineFile.db.Write(batch, nil)
	if err != nil {
		return err
	}
	engineFile.ts = ts
	local.mu.Lock()
	local.engines[engineUUID] = engineFile
	local.mu.Unlock()
	return
}

func (local *local) MakeEmptyRows() Rows {
	return kvPairs(nil)
}

func (local *local) NewEncoder(tbl table.Table, options *SessionOptions) Encoder {
	return NewTableKVEncoder(tbl, options)
}

func isIngestRetryable(resp *sst.IngestResponse, region *split.RegionInfo, meta *sst.SSTMeta) (bool, *split.RegionInfo, error) {
	if resp.GetError() == nil {
		return false, nil, nil
	}

	var newRegion *split.RegionInfo
	switch errPb := resp.GetError(); {
	case errPb.NotLeader != nil:
		if newLeader := errPb.GetNotLeader().GetLeader(); newLeader != nil {
			newRegion = &split.RegionInfo{
				Leader: newLeader,
				Region: region.Region,
			}
		return true, newRegion, errors.Errorf("not leader: %s", errPb.GetMessage())
	}
	case errPb.EpochNotMatch != nil:
		if currentRegions := errPb.GetEpochNotMatch().GetCurrentRegions(); currentRegions != nil {
			var currentRegion *metapb.Region
			for _, r := range currentRegions {
				if insideRegion(r, meta) {
					currentRegion = r
					break
				}
			}
			if currentRegion != nil {
				var newLeader *metapb.Peer
				for _, p := range currentRegion.Peers {
					if p.GetStoreId() == region.Leader.GetStoreId() {
						newLeader = p
						break
					}
				}
				if newLeader != nil {
					newRegion = &split.RegionInfo{
						Leader:newLeader,
						Region:currentRegion,
					}
				}
			}
		}
		return true, newRegion, errors.Errorf("epoch not match: %s", errPb.GetMessage())
	}
	return false, nil, errors.Errorf("non retryable error: %s", resp.GetError().GetMessage())
}

func nextKey(key []byte) []byte {
	if len(key) == 0 {
		return []byte{}
	}
	res := make([]byte, 0, len(key) + 1)
	pos := 0
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] != '\xff' {
			pos = i
			break
		}
	}
	s, e := key[:pos], key[pos] + 1
	res = append(append(res, s...), e)
	return res
}
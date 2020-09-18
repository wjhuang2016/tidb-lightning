CREATE TABLE `tbtp3at0` (
  `tbtp3at0_a` char(5) NOT NULL DEFAULT ' ',
  `tbtp3at0_b` char(7) NOT NULL DEFAULT ' ',
  `tbtp3at0_c` char(32) NOT NULL DEFAULT ' ',
  `tbtp3at0_d` smallint(6) NOT NULL DEFAULT 0,
  `tbtp3at0_e` smallint(6) NOT NULL DEFAULT 0,
  `tbtp3at0_f` char(7) NOT NULL DEFAULT ' ',
  `tbtp3at0_g` char(32) NOT NULL DEFAULT ' ',
  `tbtp3at0_h` char(6) NOT NULL DEFAULT ' ',
  UNIQUE KEY `tbtp3at0_uk` (`tbtp3at0_a`,`tbtp3at0_b`,`tbtp3at0_c`,`tbtp3at0_d`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

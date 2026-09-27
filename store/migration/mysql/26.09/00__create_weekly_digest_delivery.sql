CREATE TABLE `weekly_digest_delivery` (
  `user_id`      INT          NOT NULL,
  `period_key`   VARCHAR(32)  NOT NULL,
  `claimed_ts`   BIGINT       NOT NULL,
  `delivered_ts` BIGINT       DEFAULT NULL,
  PRIMARY KEY (`user_id`, `period_key`)
);

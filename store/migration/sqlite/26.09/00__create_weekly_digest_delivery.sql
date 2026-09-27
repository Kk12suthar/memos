CREATE TABLE weekly_digest_delivery (
  user_id      INTEGER NOT NULL,
  period_key   TEXT    NOT NULL,
  claimed_ts   BIGINT  NOT NULL,
  delivered_ts BIGINT  DEFAULT NULL,
  PRIMARY KEY (user_id, period_key)
);

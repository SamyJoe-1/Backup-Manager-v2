-- Test data for the integration run. Every tricky column type, plus a money ledger with invariants.
CREATE DATABASE shop CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE other CHARACTER SET utf8mb4;
USE shop;
CREATE TABLE accounts (id INT PRIMARY KEY, balance DECIMAL(14,2) NOT NULL, updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)) ENGINE=InnoDB;
CREATE TABLE ledger (id BIGINT AUTO_INCREMENT PRIMARY KEY, from_id INT NOT NULL, to_id INT NOT NULL, amount DECIMAL(14,2) NOT NULL, note VARCHAR(200), created_at DATETIME(3) NOT NULL,
  KEY (from_id), CONSTRAINT fk_from FOREIGN KEY (from_id) REFERENCES accounts(id)) ENGINE=InnoDB;
CREATE TABLE stats (k VARCHAR(20) PRIMARY KEY, n BIGINT NOT NULL) ENGINE=InnoDB;
CREATE TABLE audit_trail (id BIGINT AUTO_INCREMENT PRIMARY KEY, ledger_id BIGINT, at DATETIME(3)) ENGINE=InnoDB;
INSERT INTO stats VALUES ('transfers', 0);
INSERT INTO accounts (id, balance) SELECT seq, 1000 FROM seq_1_to_2000;
CREATE TRIGGER ledger_ai AFTER INSERT ON ledger FOR EACH ROW INSERT INTO audit_trail (ledger_id, at) VALUES (NEW.id, NOW(3));
CREATE VIEW rich_accounts AS SELECT id, balance FROM accounts WHERE balance > 1500;
DELIMITER ;;
CREATE PROCEDURE transfer(IN a INT, IN b INT, IN amt DECIMAL(14,2))
BEGIN
  START TRANSACTION;
  UPDATE accounts SET balance = balance - amt WHERE id = a;
  UPDATE accounts SET balance = balance + amt WHERE id = b;
  INSERT INTO ledger (from_id, to_id, amount, note, created_at) VALUES (a, b, amt, CONCAT('t ', a, '->', b), NOW(3));
  UPDATE stats SET n = n + 1 WHERE k = 'transfers';
  COMMIT;
END;;
CREATE PROCEDURE churn(IN loops INT)
BEGIN
  DECLARE i INT DEFAULT 0;
  WHILE i < loops DO
    CALL transfer(1 + FLOOR(RAND()*2000), 1 + FLOOR(RAND()*2000), ROUND(RAND()*50, 2));
    SET i = i + 1;
  END WHILE;
END;;
CREATE FUNCTION double_it(x INT) RETURNS INT DETERMINISTIC RETURN x * 2;;
DELIMITER ;
CREATE TABLE `odd types` (
  id INT AUTO_INCREMENT PRIMARY KEY, t TINYINT, ub BIGINT UNSIGNED, d DECIMAL(20,6), f FLOAT, dbl DOUBLE, b BIT(10), flag BIT(1),
  c CHAR(5), v VARCHAR(100), txt LONGTEXT, js JSON, e ENUM('a','b','c'), s SET('x','y','z'),
  bin BINARY(4), vb VARBINARY(50), bl LONGBLOB, dt DATE, dtm DATETIME(6), ts TIMESTAMP NULL, tm TIME, y YEAR,
  `weird ``col` VARCHAR(20), gen INT AS (t * 2) VIRTUAL, gens INT AS (t + 1) STORED, pt POINT
) ENGINE=InnoDB;
SET sql_mode = '';
INSERT INTO `odd types` (t, ub, d, f, dbl, b, flag, c, v, txt, js, e, s, bin, vb, bl, dt, dtm, ts, tm, y, `weird ``col`, pt) VALUES
 (-128, 18446744073709551615, -12345678901234.123456, 1.5, -2.2250738585072014e-308, b'1010101010', b'1', 'ab', 'it''s "quoted" \\ back', 'line1\nline2\r\ttab\0nul\Zsub', '{"a":[1,2,{"k":"v"}],"ar":"عربي"}', 'b', 'x,z',
  0x00FF2700, 0x27225C0A0D1A00, UNHEX(REPEAT('DEADBEEF00FF27', 5000)), '0000-00-00', '2026-11-15 13:00:00.123456', '2026-11-15 13:00:00', '-838:59:59', 2026, 'x', POINT(31.2, 30.0)),
 (NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
 (0, 0, 0, 0, 0, b'0', b'0', '', '', '', '[]', 'a', '', 0x00000000, '', '', '9999-12-31', '1000-01-01 00:00:00', '1970-01-01 00:00:01', '00:00:00', 1901, 'emoji 😀 مرحبا ✓', POINT(0,0)),
 (1, 1, 1.5, 3.4028235e38, 1.7976931348623157e308, b'1', b'1', 'x', '); DROP TABLE accounts; --', REPEAT('long text ', 30000), '{"n":null}', 'c', 'x,y,z', 'abcd', 'plain', REPEAT('a', 300000), '2026-02-28', NOW(6), NOW(), '12:34:56', 2000, NULL, NULL);
CREATE TABLE big (id INT PRIMARY KEY, payload VARCHAR(255) NOT NULL, n BIGINT NOT NULL, KEY (n)) ENGINE=InnoDB;
INSERT INTO big SELECT seq, CONCAT(MD5(seq), SHA2(seq, 256), MD5(seq * 7)), seq * 31 FROM seq_1_to_400000;
CREATE TABLE empty_table (id INT PRIMARY KEY, x TEXT) ENGINE=InnoDB;
CREATE TABLE legacy_myisam (id INT PRIMARY KEY, x VARCHAR(50)) ENGINE=MyISAM;
INSERT INTO legacy_myisam VALUES (1, 'old engine'), (2, 'still here');
USE other;
CREATE TABLE notes (id INT AUTO_INCREMENT PRIMARY KEY, body TEXT) ENGINE=InnoDB;
INSERT INTO notes (body) SELECT CONCAT('note ', seq) FROM seq_1_to_500;
-- least-privilege backup user: can read, cannot change anything
CREATE USER 'bm_backup'@'%' IDENTIFIED BY 'bm-backup-pw';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, BINLOG MONITOR, RELOAD, PROCESS, LOCK TABLES, REPLICATION SLAVE, REPLICA MONITOR, CONNECTION ADMIN ON *.* TO 'bm_backup'@'%';

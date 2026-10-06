/*M!999999\- enable the sandbox mode */
-- Hand-written synthetic dump with parser edge cases. No real data.
-- Every statement here must import into MariaDB without errors.

/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;

# A hash comment.
/* A block comment; with a semicolon and 'a quote */

DROP TABLE IF EXISTS `wp_types`;
CREATE TABLE `wp_types` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `i` int(11) DEFAULT NULL,
  `d` decimal(10,2) DEFAULT NULL,
  `f` double DEFAULT NULL,
  `b` bit(8) DEFAULT NULL,
  `bin` varbinary(32) DEFAULT NULL,
  `blob_col` blob DEFAULT NULL,
  `t` text DEFAULT NULL,
  `j` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`j`)),
  `dt` datetime DEFAULT NULL,
  `e` enum('a','b','it''s') DEFAULT NULL,
  `s` set('x','y','z') DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci COMMENT='types; with ''quotes'' and -- dashes';

LOCK TABLES `wp_types` WRITE;
/*!40000 ALTER TABLE `wp_types` DISABLE KEYS */;
INSERT INTO `wp_types` VALUES (1,-42,-1234.56,1.5e-10,b'10100101',_binary 'ab\0c',0x00FF10,'plain','{\"a\":[1,2,{\"b\":\"c;d\"}]}','2020-02-29 23:59:59','it\'s','x,z'),(2,NULL,0.00,-0,NULL,'',NULL,'','[]','0000-00-00 00:00:00','a',''),(3,2147483647,99999999.99,1e300,b'0',X'DEADBEEF',_binary '\Z\n\r\t\\','escapes: \0 \' \" \b \n \r \t \Z \\ \% \_','null','1970-01-01 00:00:01',NULL,NULL);
INSERT INTO `wp_types` VALUES (4,0,1,2,b'1',X'','','doubled \'\' quotes and \'\'\' triple','{}',NULL,'b','y'),(5,1,2,3,NULL,NULL,NULL,'unicode: café 東京 Привет 🌳 \u0000 not an escape','\"str\"',NULL,NULL,NULL);
/*!40000 ALTER TABLE `wp_types` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Tricky text that looks like SQL.
--

DROP TABLE IF EXISTS `wp_odd-name`;
CREATE TABLE `wp_odd-name` (
  `select` int(11) NOT NULL,
  `we``ird` varchar(255) NOT NULL DEFAULT '',
  `value` longtext NOT NULL,
  PRIMARY KEY (`select`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci;

INSERT INTO `wp_odd-name` (`select`, `we``ird`, `value`) VALUES (1,'a;b','),(\'2\',\'x\');'),(2,'-- not a comment','/* not a comment */'),(3,'# not a comment','DELIMITER ;;'),(4,'back\\','slash at end \\'),(5,'`backtick`','INSERT INTO `wp_odd-name` VALUES (9,\'x\',\'y\');');
INSERT INTO `wp_odd-name` VALUES
(6,'multi-line','one'),
(7,'multi-line','two'),
  (8 , 'spaced' , 'three' )
;
INSERT INTO `wp_odd-name` (`select`,`value`) VALUES (9,'column subset');
INSERT IGNORE INTO `wp_odd-name` VALUES (1,'ignored','duplicate key is ignored');
REPLACE INTO `wp_odd-name` VALUES (2,'replaced','row 2 replaced');
INSERT INTO `wp_odd-name` VALUES (3,'dup','dup') ON DUPLICATE KEY UPDATE `value`='updated; on duplicate';
INSERT INTO `wp_odd-name` VALUES (10,CONCAT('con','cat'),REPEAT('ab',3)),(11,1+1,LOWER('X'));
insert into `wp_odd-name` values (12,'lower case','keywords');
INSERT INTO `wp_odd-name` VALUES (13,'crlf line ending','x');
INSERT INTO `wp_odd-name` SELECT `select`+100, `we``ird`, `value` FROM `wp_odd-name` WHERE `select` < 3;

--
-- Empty table.
--

DROP TABLE IF EXISTS `wp_empty`;
CREATE TABLE `wp_empty` (
  `id` int(11) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci;

--
-- A view, as mysqldump writes it.
--

/*!50001 DROP VIEW IF EXISTS `wp_view`*/;
/*!50001 CREATE ALGORITHM=UNDEFINED */
/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */
/*!50001 VIEW `wp_view` AS select `wp_odd-name`.`select` AS `id`,`wp_odd-name`.`value` AS `value` from `wp_odd-name` where `wp_odd-name`.`value` <> ';' */;

--
-- A routine with semicolons in its body.
--

/*!50003 DROP PROCEDURE IF EXISTS `wp_count` */;
DELIMITER ;;
CREATE DEFINER=`root`@`localhost` PROCEDURE `wp_count`(OUT n INT)
BEGIN
  -- a comment inside the body; with a semicolon
  SELECT COUNT(*) INTO n FROM `wp_odd-name`;
  SET n = n + 0;
END ;;
DELIMITER ;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;
/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;

-- Dump completed

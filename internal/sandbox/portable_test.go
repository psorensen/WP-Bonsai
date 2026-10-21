package sandbox

import (
	"bytes"
	"strings"
	"testing"
)

func TestWritePortableWrapsLiteralDefaults(t *testing.T) {
	in := "CREATE TABLE `wp_example_scores` (\n" +
		"  `id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
		"  `label` varchar(20) NOT NULL DEFAULT '0',\n" +
		"  `notes` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT _utf8mb4'0',\n" +
		"  `title` text NOT NULL DEFAULT _utf8mb4'',\n" +
		"  `quote` mediumtext NOT NULL DEFAULT 'it''s \\' DEFAULT x' COMMENT 'a DEFAULT b',\n" +
		"  `count` tinytext NOT NULL DEFAULT 5,\n" +
		"  `extra` longtext DEFAULT NULL,\n" +
		"  `payload` blob NOT NULL DEFAULT ('x'),\n" +
		"  `data` json NOT NULL DEFAULT '{}',\n" +
		"  `textual` varchar(10) NOT NULL DEFAULT 'text',\n" +
		"  `body` longtext NOT NULL,\n" +
		"  PRIMARY KEY (`id`)\n" +
		") ENGINE=InnoDB;\n" +
		"INSERT INTO `wp_example_scores` VALUES (1,'0','  `x` text NOT NULL DEFAULT ''0''');\n"
	want := "CREATE TABLE `wp_example_scores` (\n" +
		"  `id` bigint(20) unsigned NOT NULL DEFAULT 0,\n" +
		"  `label` varchar(20) NOT NULL DEFAULT '0',\n" +
		"  `notes` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT (_utf8mb4'0'),\n" +
		"  `title` text NOT NULL DEFAULT (_utf8mb4''),\n" +
		"  `quote` mediumtext NOT NULL DEFAULT ('it''s \\' DEFAULT x') COMMENT 'a DEFAULT b',\n" +
		"  `count` tinytext NOT NULL DEFAULT (5),\n" +
		"  `extra` longtext DEFAULT NULL,\n" +
		"  `payload` blob NOT NULL DEFAULT ('x'),\n" +
		"  `data` json NOT NULL DEFAULT ('{}'),\n" +
		"  `textual` varchar(10) NOT NULL DEFAULT 'text',\n" +
		"  `body` longtext NOT NULL,\n" +
		"  PRIMARY KEY (`id`)\n" +
		") ENGINE=InnoDB;\n" +
		"INSERT INTO `wp_example_scores` VALUES (1,'0','  `x` text NOT NULL DEFAULT ''0''');\n"
	var out bytes.Buffer
	if err := writePortable(&out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Errorf("writePortable =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestWritePortablePassesLongLines(t *testing.T) {
	long := "INSERT INTO `wp_example` VALUES ('" + strings.Repeat("a", 300*1024) + "');\n"
	in := long + "  `t` text NOT NULL DEFAULT '',\n" + "no newline at end"
	var out bytes.Buffer
	if err := writePortable(&out, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	want := long + "  `t` text NOT NULL DEFAULT (''),\n" + "no newline at end"
	if out.String() != want {
		t.Errorf("long line output differs: got %d bytes, want %d", out.Len(), len(want))
	}
}

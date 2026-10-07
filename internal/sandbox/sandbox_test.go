package sandbox

import (
	"strings"
	"testing"
)

func TestDBErrorsRedacts(t *testing.T) {
	out := "--------------\nINSERT INTO `wp_2_options` VALUES (1,'jpsq_sync-1','a:1:{s:10:\"user_email\";s:17:\"pat@example-news.com\";}','no')\n--------------\n\n" +
		"ERROR 1114 (HY000) at line 11588: The table 'wp_2_options' is full\n"
	got := dbErrors(out)
	if got != "ERROR 1114 (HY000) at line 11588: The table 'wp_2_options' is full" {
		t.Errorf("dbErrors = %q", got)
	}
	dup := dbErrors("ERROR 1062 (23000) at line 9: Duplicate entry 'pat@example-news.com' for key 'user_email'")
	if strings.Contains(dup, "pat@") || !strings.Contains(dup, "'user_email'") {
		t.Errorf("duplicate entry error = %q", dup)
	}
	if got := Redact("Error: user pat@example-news.com exists, display name 'Pat Smith'"); strings.Contains(got, "pat@") || strings.Contains(got, "Pat Smith") {
		t.Errorf("Redact = %q", got)
	}
	if dbErrors("some noise") != "no error message from MariaDB" {
		t.Error("no ERROR line should give a fixed message")
	}
}

package sqldump

import (
	"bytes"
	"os"
	"testing"
)

// FuzzVerbatim checks that any input the parser accepts comes back byte for
// byte, and that every row body is a parenthesized tuple.
func FuzzVerbatim(f *testing.F) {
	seeds := []string{
		"INSERT INTO `t` VALUES (1,'a'),(2,'b');\n",
		"CREATE TABLE `t` (`a` int);\n/*!40101 SET x=1 */;\n",
		"DELIMITER ;;\nCREATE TRIGGER x BEGIN SET a=1; END;;\nDELIMITER ;\n",
		"INSERT INTO t VALUES (_binary 'a\\'b',0xFF,NULL,-1.5e3) ON DUPLICATE KEY UPDATE a=1;",
		"-- c\n# d\n/* e */\n",
	}
	if b, err := os.ReadFile("../../testdata/edge-cases.sql"); err == nil {
		seeds = append(seeds, string(b))
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		items, err := tryParseAll(input)
		if err != nil {
			return
		}
		if !bytes.Equal(joinRaw(items), input) {
			t.Fatalf("round trip differs for %q", input)
		}
		for _, it := range items {
			if it.Kind == Row && (len(it.Body) < 2 || it.Body[0] != '(' || it.Body[len(it.Body)-1] != ')') {
				t.Fatalf("row body %q is not a tuple", it.Body)
			}
		}
	})
}

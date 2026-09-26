// Package hack holds tests that keep the shell harnesses in step with the
// Go code they exercise.
package hack

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/middlendian/fileblock-csi/pkg/image"
)

// smoke.sh can't import image.DefaultBlockSize, so it mirrors it in one
// variable; this keeps the two equal and keeps the value from reappearing
// as a bare literal elsewhere in the script.
func TestSmokeBlockSizeMatches(t *testing.T) {
	b, err := os.ReadFile("smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	m := regexp.MustCompile(`(?m)^DEFAULT_BLOCK_SIZE=(\d+)$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("smoke.sh does not define DEFAULT_BLOCK_SIZE")
	}
	if m[1] != strconv.Itoa(image.DefaultBlockSize) {
		t.Fatalf("smoke.sh DEFAULT_BLOCK_SIZE=%s, image.DefaultBlockSize=%d", m[1], image.DefaultBlockSize)
	}
	literal := regexp.MustCompile(`\b` + m[1] + `\b`)
	if n := len(literal.FindAllString(script, -1)); n != 1 {
		t.Fatalf("smoke.sh has %d bare %s literals; reference $DEFAULT_BLOCK_SIZE instead", n-1, m[1])
	}
	if !strings.Contains(script, "$DEFAULT_BLOCK_SIZE") {
		t.Fatal("smoke.sh never references $DEFAULT_BLOCK_SIZE")
	}
}

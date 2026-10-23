package ghaction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteOutputs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", p)
	evil := "x\nother=injected\nghadelimiter_"
	if err := WriteOutputs(map[string]string{"release-url": "https://x", "assets": evil}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	lines := strings.Split(string(data), "\n")
	if !strings.HasPrefix(lines[0], "assets<<ghadelimiter_") {
		t.Fatalf("output file:\n%s", data)
	}
	delim := strings.TrimPrefix(lines[0], "assets<<")
	if lines[4] != delim || lines[1] != "x" || lines[2] != "other=injected" {
		t.Errorf("value not enclosed:\n%s", data)
	}
	if err := WriteOutputs(map[string]string{"bad name": "x"}); err == nil {
		t.Error("invalid name accepted")
	}
}

func TestSummaryAndTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", p)
	if err := AppendSummary(strings.Repeat("x", MaxSummary+10)); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Size() > MaxSummary {
		t.Errorf("summary size %d", st.Size())
	}
	got := Table([]string{"a"}, [][]string{{"x|y<script>\n"}})
	if !strings.Contains(got, `x\|y&lt;script&gt; `) {
		t.Errorf("table = %q", got)
	}
}

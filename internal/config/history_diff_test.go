package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoryDiffRedactsSecretsAndReportsSecretOnlyChanges(t *testing.T) {
	old := Default()
	old.Runtime.Args = []string{"--api-key", "old-secret", "--token=old-token"}
	next := Clone(old)
	next.Runtime.Args = []string{"--api-key", "new-secret", "--token=new-token"}
	changes := HistoryDiff(old, next)
	if len(changes) != 1 || changes[0].Path != "runtime.args" {
		t.Fatalf("unexpected changes: %#v", changes)
	}
	b, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"old-secret", "new-secret", "old-token", "new-token"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("history contains secret: %s", b)
		}
	}
	if !strings.Contains(string(b), `\u003credacted\u003e`) {
		t.Fatalf("missing redaction: %s", b)
	}
}

func TestHistoryDiffOrdersPaths(t *testing.T) {
	old := Default()
	next := Clone(old)
	next.Timezone = "UTC"
	next.Runtime.ModelPath = "model.gguf"
	changes := HistoryDiff(old, next)
	if len(changes) != 2 || changes[0].Path != "runtime.model_path" || changes[1].Path != "timezone" {
		t.Fatalf("unexpected changes: %#v", changes)
	}
}

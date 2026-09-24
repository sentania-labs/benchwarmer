package api

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

// TestOpenAPIMatchesWireTypes keeps docs/api/openapi.yaml in step with the
// Go wire types: each schema's property names must equal the struct's JSON
// field names exactly. It relies on the document's layout (schemas at four
// spaces, properties at eight), not a YAML parser.
func TestOpenAPIMatchesWireTypes(t *testing.T) {
	b, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	schemas := map[string]any{
		"Error": Error{}, "FieldError": config.FieldError{}, "Health": Health{}, "Status": Status{},
		"ModeStatus": ModeStatus{}, "ProfileStatus": ProfileStatus{}, "RuntimeStatus": RuntimeStatus{},
		"GPUStatus": GPUStatus{}, "TimerStatus": TimerStatus{}, "AgentStatus": AgentStatus{},
		"Decision": policy.Decision{}, "Evidence": policy.Evidence{}, "AppMatch": policy.AppMatch{},
		"Event": events.Event{}, "EventTelemetry": events.Telemetry{}, "EventsResponse": EventsResponse{},
		"ModeRequest": ModeRequest{}, "ActionRequest": ActionRequest{}, "ActionResponse": ActionResponse{},
		"AgentReport": AgentReport{}, "ConfigResponse": ConfigResponse{}, "Impact": config.Impact{},
		"ApplicationsBody": ApplicationsBody{}, "Config": config.Config{}, "RuntimeConfig": config.Runtime{},
		"ListenConfig": config.Listen{}, "TelemetryConfig": config.Telemetry{}, "SafetyConfig": config.Safety{},
		"ContentionConfig": config.Contention{}, "Profile": config.Profile{}, "Schedule": config.Schedule{},
		"AppRule": config.AppRule{}, "AntiThrashConfig": config.AntiThrash{}, "RecoveryConfig": config.Recovery{},
		"ModesConfig": config.Modes{}, "SignalsConfig": config.Signals{}, "SecurityConfig": config.Security{},
		"RetentionConfig": config.Retention{}, "MetricsConfig": config.Metrics{}, "LoggingConfig": config.Logging{},
	}
	for name, v := range schemas {
		got := schemaProps(t, doc, name)
		want := jsonFields(reflect.TypeOf(v))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("schema %s properties\n got  %v\n want %v", name, got, want)
		}
	}
	for _, code := range []string{CodeUnauthorized, CodeInvalidToken, CodeForbidden, CodeNotFound, CodeMethodNotAllowed,
		CodeBadRequest, CodeBodyTooLarge, CodeInvalidConfig, CodeInvalidMode, CodeInternal, CodeRejected, CodeWorkerUnavailable} {
		if !strings.Contains(doc, "        - "+code+"\n") {
			t.Errorf("ErrorCode enum lacks %q", code)
		}
	}
}

var propLine = regexp.MustCompile(`^        ([a-z_]+):`)

func schemaProps(t *testing.T, doc, name string) []string {
	t.Helper()
	start := strings.Index(doc, "\n    "+name+":\n")
	if start < 0 {
		t.Errorf("schema %s missing", name)
		return nil
	}
	lines := strings.Split(doc[start+1:], "\n")[1:]
	var props []string
	inProps := false
	for _, l := range lines {
		if strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "     ") {
			break // next schema
		}
		if l == "      properties:" {
			inProps = true
			continue
		}
		if strings.HasPrefix(l, "      ") && !strings.HasPrefix(l, "       ") {
			inProps = false
		}
		if m := propLine.FindStringSubmatch(l); inProps && m != nil {
			props = append(props, m[1])
		}
	}
	sort.Strings(props)
	return props
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

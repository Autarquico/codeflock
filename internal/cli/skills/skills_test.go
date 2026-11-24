package skills_test

import (
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/cli/skills"
)

func TestCodeflockCarriesTheVersionAndNamesEveryTool(t *testing.T) {
	b, err := skills.Codeflock("0.5.0-beta.1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, skills.Marker+"0.5.0-beta.1 -->") || strings.Contains(text, "{{") {
		t.Error("version not written into the marker")
	}
	for _, tool := range []string{"peers_list", "jobs_list", "jobs_get", "jobs_run", "jobs_screen", "jobs_send", "jobs_stop", "jobs_resume", "jobs_attach"} {
		if !strings.Contains(text, tool) {
			t.Errorf("the skill does not mention %s", tool)
		}
	}
}

func TestCodeflockRefusesAVersionThatBreaksTheFile(t *testing.T) {
	if _, err := skills.Codeflock("1 --> <!--"); err == nil {
		t.Error("accepted a version with markup")
	}
}

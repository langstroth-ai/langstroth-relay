package relay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Every command's help has the shared sections in the shared order, and the
// top-level help follows the same order.
func TestHelpSectionOrder(t *testing.T) {
	order := []string{"NAME", "USAGE", "FLAGS", "EXAMPLES", "ENVIRONMENT", "EXIT CODES", "SEE ALSO"}
	for _, c := range commandHelps {
		h := helpText(t, c.Name)
		at := -1
		for _, sec := range order {
			i := strings.Index(h, "\n"+sec+"\n")
			if sec == "NAME" {
				i = strings.Index(h, sec+"\n")
			}
			if i < 0 || i < at {
				t.Errorf("help %s: %s missing or out of order:\n%s", c.Name, sec, h)
			}
			at = i
		}
		if len(c.Examples) == 0 || len(c.SeeAlso) == 0 {
			t.Errorf("help %s: no examples or see also", c.Name)
		}
	}
	var b bytes.Buffer
	_ = cmdHelp(nil, &b)
	at := -1
	for _, sec := range []string{"NAME", "USAGE", "COMMANDS", "EXAMPLES", "ENVIRONMENT", "EXIT CODES", "SEE ALSO"} {
		i := strings.Index(b.String(), sec+"\n")
		if i < 0 || i < at {
			t.Errorf("top-level help: %s missing or out of order", sec)
		}
		at = i
	}
	// Cross-references to the Ligus side.
	if !strings.Contains(helpText(t, "check"), "ligus doctor") || !strings.Contains(b.String(), "ligus help checks") {
		t.Error("help names no ligus doctor / ligus help checks")
	}
}

func TestHelpJSON(t *testing.T) {
	for _, args := range [][]string{{"--json"}, {"-o", "json"}} {
		var b bytes.Buffer
		if err := cmdHelp(args, &b); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Schema   string
			Commands []commandHelp
			Topics   []struct{ Name, Summary string }
		}
		if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
		if doc.Schema != "ligus.help.v1" || len(doc.Commands) != 4 || len(doc.Topics) < 7 {
			t.Errorf("help %v: %+v", args, doc)
		}
		for _, tp := range doc.Topics {
			if findCommandHelp(tp.Name) != nil {
				t.Errorf("command %s listed as a topic too", tp.Name)
			}
		}
	}
}

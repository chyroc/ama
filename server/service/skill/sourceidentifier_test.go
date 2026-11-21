package skill

import (
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

func TestSplitSourceIdentifier(t *testing.T) {
	for _, input := range []string{"skills:example/skill", "example/skill", "/example/skill/"} {
		namespace, name, err := SplitSourceIdentifier(input)
		if err != nil || namespace != "example" || name != "skill" {
			t.Errorf("SplitSourceIdentifier(%q) = %q, %q, %v", input, namespace, name, err)
		}
	}
	if _, _, err := SplitSourceIdentifier("missing-slash"); !errors.Is(err, persistmodel.ErrInvalidArgument) {
		t.Fatalf("invalid identifier error = %v", err)
	}
}

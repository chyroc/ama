package userfile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"

	"github.com/chyroc/ama/server/model/persistmodel"
)

type uploadOnlyObjects struct{ ObjectStore }

func TestOpenFileWithoutStreamingSupport(t *testing.T) {
	objects := &uploadOnlyObjects{ObjectStore: newFakeObjects()}
	svc, err := New(Config{Store: newFakeStore(), Objects: objects})
	if err != nil {
		t.Fatal(err)
	}
	started, err := svc.StartUpload(context.Background(), "owner@example.com", apimodel.StartUserFileUpload{
		Filename: "test.txt", ContentType: "text/plain", Size: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutSimpleContent(context.Background(), "owner@example.com", started.ID, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if _, body, err := svc.OpenFile(context.Background(), "owner@example.com", started.ID); !errors.Is(err, persistmodel.ErrPrecondition) {
		t.Fatalf("missing ObjectOpener should be an explicit precondition error, got %v", err)
	} else if body != nil {
		_ = body.Close()
		t.Fatal("failed OpenFile must not return a body")
	}
}

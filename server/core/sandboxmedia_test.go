package core

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadRawSandboxFileWithResume(t *testing.T) {
	var calls []string
	file, err := ReadRawSandboxFileWithResume(context.Background(), "/work/demo.png", 32, false,
		func(context.Context) error {
			calls = append(calls, "prepare")
			return nil
		}, func(context.Context) (io.ReadCloser, error) {
			calls = append(calls, "open")
			return io.NopCloser(strings.NewReader("image")), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Data) != "image" || strings.Join(calls, ",") != "prepare,open" {
		t.Fatalf("准备和读取顺序错误: data=%q calls=%v", file.Data, calls)
	}
}

func TestReadRawSandboxFileWithResumeFailureDoesNotOpen(t *testing.T) {
	want := errors.New("executor unavailable")
	_, err := ReadRawSandboxFileWithResume(context.Background(), "/work/demo.png", 32, false,
		func(context.Context) error { return want },
		func(context.Context) (io.ReadCloser, error) {
			t.Fatal("恢复失败时不能继续读取")
			return nil, nil
		})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v want=%v", err, want)
	}
}

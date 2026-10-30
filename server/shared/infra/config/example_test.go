package config

import (
	"os"
	"testing"
)

// TestExampleConfigHasNoUnknownFields: every key in the template must be one the struct knows.
//
// The template is the starting point users copy: if it keeps a section that has been removed
// (for example config of an old executor), parsing does not fail, it only adds a warning line in
// the startup log, and whoever copies it will think the key still does something.
func TestExampleConfigHasNoUnknownFields(t *testing.T) {
	raw, err := os.ReadFile("../../../config.example.yaml")
	if err != nil {
		t.Fatalf("读模板失败: %v", err)
	}
	res, err := ParseLayers("example", raw)
	if err != nil {
		t.Fatalf("模板解析失败: %v", err)
	}
	if len(res.UnknownFields) > 0 {
		t.Errorf("模板里有结构体不认识的键: %v", res.UnknownFields)
	}
	if _, err := res.Config.Docker.MemoryBytes(); err != nil {
		t.Errorf("模板里的 docker.memory 解析不了: %v", err)
	}
}

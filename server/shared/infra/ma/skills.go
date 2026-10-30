// Skill: a reusable skill package, uploaded as a file (zip) and then referenced by Agents.
// API: /api/v3/skills
//
// Note: creating a Skill is a multipart file upload, not a JSON request body, so the signatures
// look different from the other resources. The current SDK (v0.4.0) only offers create and get.

package ma

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/skill"
)

// SkillService uploads and queries skill packages. Get it via Instance.Skills().
type SkillService struct {
	client *arkruntime.Client
}

// Create uploads a skill package from any io.Reader.
//
// fileName is the uploaded file name (the server uses it to detect the format); displayTitle is
// the display name.
func (s *SkillService) Create(
	ctx context.Context,
	file io.Reader,
	fileName, displayTitle string,
) (*skill.Skill, error) {
	if file == nil {
		return nil, errors.New("ma: file 不能为 nil")
	}
	if fileName == "" {
		return nil, errors.New("ma: fileName 必填")
	}
	return s.client.CreateSkill(ctx, file, fileName, displayTitle)
}

// CreateFromPath uploads a skill package from a local file path. It is a convenience wrapper
// around Create.
//
// When displayTitle is empty, the file name (without extension) is used as the display name.
func (s *SkillService) CreateFromPath(
	ctx context.Context,
	path, displayTitle string,
) (*skill.Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	name := filepath.Base(path)
	if displayTitle == "" {
		displayTitle = name[:len(name)-len(filepath.Ext(name))]
	}
	return s.Create(ctx, f, name, displayTitle)
}

// Get returns skill package details by ID, including the latest version number.
func (s *SkillService) Get(ctx context.Context, skillID string) (*skill.Skill, error) {
	return s.client.GetSkill(ctx, skillID)
}

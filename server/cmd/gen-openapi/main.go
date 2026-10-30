// gen-openapi writes the OpenAPI document to server/openapi/openapi.json.
//
// # Why the spec is a build output checked into the repo
//
// It is generated from the route permission table in authz/matrix.go, and could be computed at
// request time. We write it to a file and check it in so that "the API changed" is **visible in
// the commit history**: it can be diffed, code reviewed, and checked in CI. If it were generated
// at request time, renaming a route would leave no trace.
//
// The cost is that it can go stale (a route changed but nobody regenerated it). A machine check
// covers that cost: openapi.TestSpecFileIsUpToDate regenerates it and compares it byte for byte
// with the file, and fails if they differ. So the flow is:
//
//	change route -> must write matrix (otherwise the service will not start) -> run this command -> commit
//
// If you forget to run this command the test fails, and the error message gives the command to run.
//
// # Usage
//
//	cd server && go run ./cmd/gen-openapi     # write the file
//	cd server && go generate ./openapi        # same as above (openapi/handler.go has go:generate)
//	cd server && go run ./cmd/gen-openapi -check   # only check whether it is up to date, do not write
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chyroc/ama/server/openapi"
)

// specPath is the output path, relative to the server/ directory.
//
// Hard-coded instead of taken as a flag: the location of this file is part of the contract
// (openapi/handler.go references it with go:embed). If it were configurable there would be two
// sources of truth.
const specPath = "openapi/openapi.json"

func main() {
	check := flag.Bool("check", false, "只检查文件是否与生成结果一致，不写入。不一致时退出码非零。")
	flag.Parse()

	generated, err := openapi.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成 OpenAPI 文档失败: %v\n", err)
		os.Exit(1)
	}

	// Allow running from any directory: locate server/ first. That way `go run ./cmd/gen-openapi`
	// and `go generate ./openapi` (whose working directory is openapi/) both end up on the same file.
	target, err := resolveTarget()
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位产物路径失败: %v\n", err)
		os.Exit(1)
	}

	if *check {
		existing, err := os.ReadFile(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n\n请先跑一次：cd server && go run ./cmd/gen-openapi\n", target, err)
			os.Exit(1)
		}
		if !bytes.Equal(existing, generated) {
			fmt.Fprintf(os.Stderr,
				"❌ %s 与 authz/matrix.go 不一致（路由或 schema 改过了，但 spec 没重新生成）。\n\n"+
					"修复：cd server && go run ./cmd/gen-openapi && git add %s\n",
				specPath, specPath)
			os.Exit(1)
		}
		fmt.Printf("✅ %s 是最新的\n", specPath)
		return
	}

	if err := os.WriteFile(target, generated, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", target, err)
		os.Exit(1)
	}
	fmt.Printf("✅ 已生成 %s（%d 字节）\n", specPath, len(generated))
}

// resolveTarget finds the actual path of server/openapi/openapi.json.
//
// It walks up from the current directory looking for go.mod: the working directory of
// `go run ./cmd/gen-openapi` is server/, while the working directory of `go generate ./openapi`
// is server/openapi/. Both must work. Without this step the latter would create a new file under
// openapi/openapi/, **and report no error**.
func resolveTarget() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, specPath), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 往上没找到 go.mod，请在 server/ 目录下运行", dir)
		}
		dir = parent
	}
}

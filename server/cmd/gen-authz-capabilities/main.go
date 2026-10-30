// gen-authz-capabilities writes the role permission table into web/src/data/roleCapabilities.ts.
//
// # Why this output lives in web/ and not server/
//
// Its only consumer is that table in the frontend. Putting it in server/ and having the frontend
// import across directories does not work: Vite's `@` alias points at `web/src`, and leaving that
// directory needs extra resolve config, while on the tsc side `rootDir` does not reach it either.
// Both would need changes, just for one data file.
//
// So this is the repo's second "Go writes, frontend reads" pipeline (the first is
// scripts/export-seed/, in the other direction: frontend writes, Go reads). Both are guarded by a
// committed output file plus a consistency test.
//
// # Usage
//
//	cd server && go run ./cmd/gen-authz-capabilities          # write the file
//	cd server && go run ./cmd/gen-authz-capabilities -check   # check only, do not write
//
// If you forget to run it: `go test ./shared/gopkg/authzdoc/` fails, and the error includes the
// fix command.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/chyroc/ama/server/shared/gopkg/authzdoc"
)

func main() {
	check := flag.Bool("check", false, "只检查文件是否与生成结果一致，不写入。不一致时退出码非零。")
	flag.Parse()

	generated, err := authzdoc.Generate()
	if err != nil {
		// The most common way to hit this path is "added a new route but did not categorize
		// it", and the Capabilities() error already says how to fix it, so print it as is.
		fmt.Fprintf(os.Stderr, "生成角色权限说明表失败：\n%v\n", err)
		os.Exit(1)
	}

	target, err := authzdoc.ResolveTarget()
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位产物路径失败: %v\n", err)
		os.Exit(1)
	}

	if *check {
		existing, readErr := os.ReadFile(target)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n\n请先跑一次：cd server && go run ./cmd/gen-authz-capabilities\n",
				authzdoc.RelPath, readErr)
			os.Exit(1)
		}
		if !bytes.Equal(existing, generated) {
			fmt.Fprintf(os.Stderr,
				"❌ %s 与 authz/matrix.go 不一致（路由或说明改过了，但表没重新生成）。\n\n"+
					"修复：cd server && go run ./cmd/gen-authz-capabilities && git add ../%s\n",
				authzdoc.RelPath, authzdoc.RelPath)
			os.Exit(1)
		}
		fmt.Printf("✅ %s 是最新的\n", authzdoc.RelPath)
		return
	}

	if err := os.WriteFile(target, generated, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", target, err)
		os.Exit(1)
	}
	fmt.Printf("✅ 已生成 %s（%d 字节）\n", authzdoc.RelPath, len(generated))
}

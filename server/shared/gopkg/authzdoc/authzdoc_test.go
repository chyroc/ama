// Consistency checks for the permission table: the output is up to date, and what it describes
// matches the matrix.
//
// # Why this file exists: make "no drift" machine-enforced
//
// Relying on people to remember "added a route, update the table" is not enough, and forgetting
// is very hard to notice: the table **looks complete**, it is just missing one capability. A user
// reads it and concludes "I cannot do this", an admin reads it and concludes "it is all
// explained", and neither notices a missing row.
//
// Three things work together:
//
//	inside Capabilities()          cannot map to a feature area / unclaimed combination → error (first test below)
//	TestGeneratedFileIsUpToDate    the output really was regenerated and committed
//	TestOwnerAndAdminAreIdentical  pins the counter-intuitive fact so nobody "fixes" it in passing

package authzdoc

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/authz"
)

// TestEveryRouteIsCovered is **the most important one**: every route in the matrix that needs a
// permission is covered by the table.
//
// It only calls Capabilities() once; the coverage check lives in that function (it checks both
// directions), because the generator also relies on it to block bad input, so it cannot be only
// in the test.
//
// # How this test goes red
//
// Add a new route to matrix.go (for example `POST /api/workspaces/{workspaceId}/exports`). It
// maps to the "workspace" area but lands on an unclaimed (area, level) combination, so this test
// fails and the error says to add a row in capability.go. **Verified by mutation testing.**
func TestEveryRouteIsCovered(t *testing.T) {
	caps, err := authz.Capabilities()
	if err != nil {
		t.Fatalf("权限说明表推导失败：\n%v", err)
	}
	if len(caps) == 0 {
		t.Fatal("权限说明表是空的")
	}

	// Also pin "each route is covered by exactly one row": Capabilities already checks for
	// duplicate claims; here we count again from the route side. Only when both sides agree do
	// we know nothing is missing and nothing is duplicated.
	seen := map[string]string{}
	for _, c := range caps {
		for _, r := range c.Routes {
			if prev, dup := seen[r]; dup {
				t.Errorf("路由 %q 同时出现在 %q 与 %q 两行里", r, prev, c.ID)
			}
			seen[r] = c.ID
		}
	}

	want := 0
	for _, r := range authz.Routes() {
		if r.Action == "none" {
			continue
		}
		want++
		if _, ok := seen[r.Method+" "+r.Path]; !ok {
			t.Errorf("路由 %s %s 要 %s 权限，但没有出现在说明表的任何一行里",
				r.Method, r.Path, r.Action)
		}
	}
	if len(seen) != want {
		t.Errorf("覆盖的路由数 %d 与 matrix 里非 none 的路由数 %d 不一致", len(seen), want)
	}
	t.Logf("说明表 %d 行，覆盖 %d 条要权限的路由", len(caps), len(seen))
}

// TestOwnerAndAdminAreIdentical pins "owner and admin have exactly the same permissions".
//
// # Why it has its own test
//
// This fact is **counter-intuitive**: "the owner surely has more permission than an admin" is a
// strong intuition. When those two columns look identical, the next person will likely think
// something is missing and "fix" it by giving owner a few more check marks. That is not fixing a
// typo, it is breaking a correct table.
//
// So this asserts they are identical and puts the reason in the failure message: if you really
// want to change it, **change authz.levelOf first** (give owner its own higher level), not this
// table; the table is computed from there.
func TestOwnerAndAdminAreIdentical(t *testing.T) {
	caps, err := authz.Capabilities()
	if err != nil {
		t.Fatalf("权限说明表推导失败：%v", err)
	}
	for _, c := range caps {
		hasOwner, hasAdmin := false, false
		for _, r := range c.Roles {
			switch r {
			case "owner":
				hasOwner = true
			case "admin":
				hasAdmin = true
			}
		}
		if hasOwner != hasAdmin {
			t.Errorf("能力 %q 的 owner(%v) 与 admin(%v) 不一致。\n"+
				"**owner 与 admin 的权限面必须完全相同**（authz.levelOf 里两者同为 "+
				"LevelAdmin，owner 只多一条「不能被移除」的保护）。\n"+
				"真要给 owner 更高的权限，改的是 authz.levelOf，不是这张表——"+
				"表是从那里算出来的。", c.ID, hasOwner, hasAdmin)
		}
	}
}

// TestRolesAreConsistentWithMinRole checks that Roles and MinRole say the same thing.
//
// Both fields describe one fact (one as a set, one as a lower bound). If they disagree, the
// table the frontend renders contradicts the "needs at least role X" line under it, and both
// come from this file.
func TestRolesAreConsistentWithMinRole(t *testing.T) {
	caps, err := authz.Capabilities()
	if err != nil {
		t.Fatalf("权限说明表推导失败：%v", err)
	}
	order := authz.RoleOrder() // from highest to lowest
	for _, c := range caps {
		// Roles must be a prefix of order (the N highest roles). A gap in the middle would mean
		// "admin can, member cannot, viewer can again", and no pattern can be read from that.
		for i, r := range c.Roles {
			if r != order[i] {
				t.Errorf("能力 %q 的 roles=%v 不是「权限从高到低的前缀」（角色顺序 %v）",
					c.ID, c.Roles, order)
				break
			}
		}
		if len(c.Roles) == 0 {
			t.Errorf("能力 %q 没有任何角色能做，那它不该出现在表里", c.ID)
			continue
		}
		if last := c.Roles[len(c.Roles)-1]; last != c.MinRole {
			t.Errorf("能力 %q 的 minRole=%q，但 roles 里权限最低的是 %q", c.ID, c.MinRole, last)
		}
	}
}

// TestEveryCapabilityHasStableID checks that ids are non-empty and unique.
//
// The id is the basis for the frontend React key and e2e selectors; duplicates make React
// silently reuse the wrong node.
func TestEveryCapabilityHasStableID(t *testing.T) {
	caps, err := authz.Capabilities()
	if err != nil {
		t.Fatalf("权限说明表推导失败：%v", err)
	}
	seen := map[string]bool{}
	for _, c := range caps {
		if c.ID == "" {
			t.Errorf("有一行没有 id（label=%q）", c.Label)
		}
		if c.Label == "" {
			t.Errorf("能力 %q 没有 label，界面上会显示成一行空白", c.ID)
		}
		if seen[c.ID] {
			t.Errorf("id %q 重复", c.ID)
		}
		seen[c.ID] = true
	}
}

// TestCapabilitiesAreSortedByRole checks that the table is sorted by "lowest role, low to high".
//
// The order is for people: once users find their own level, everything above is allowed and
// everything below is not. Out of order, they would have to check row by row, and the table
// would be pointless.
func TestCapabilitiesAreSortedByRole(t *testing.T) {
	caps, err := authz.Capabilities()
	if err != nil {
		t.Fatalf("权限说明表推导失败：%v", err)
	}
	// More roles = lower bar, so len(Roles) must be non-increasing.
	for i := 1; i < len(caps); i++ {
		if len(caps[i].Roles) > len(caps[i-1].Roles) {
			t.Errorf("第 %d 行 %q（%d 个角色）比上一行 %q（%d 个角色）门槛更低，顺序反了。\n"+
				"表必须按「最低角色从低到高」排，否则用户得逐行核对。",
				i, caps[i].ID, len(caps[i].Roles), caps[i-1].ID, len(caps[i-1].Roles))
		}
	}
}

// TestGeneratedFileIsUpToDate checks that the roleCapabilities.ts in git matches the freshly
// generated output byte by byte.
//
// **This is the last link of the whole loop.** The tests above guarantee "the table covers all
// routes"; this one guarantees "the generated output was actually committed". Otherwise a route
// changes and the table changes too, but only the in-memory copy, and the frontend still imports
// the old one.
func TestGeneratedFileIsUpToDate(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatalf("生成失败：\n%v", err)
	}

	target, err := ResolveTarget()
	if err != nil {
		t.Fatalf("定位产物路径失败：%v", err)
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v\n\n修复：cd server && go run ./cmd/gen-authz-capabilities",
			RelPath, err)
	}

	if !bytes.Equal(onDisk, generated) {
		t.Errorf("%s 已过期：authz/matrix.go 或 capability.go 改过了，但表没重新生成。\n\n"+
			"修复：cd server && go run ./cmd/gen-authz-capabilities && git add ../%s\n\n"+
			"（文件 %d 字节，重新生成 %d 字节）",
			RelPath, RelPath, len(onDisk), len(generated))
	}
}

// TestGenerateIsDeterministic checks that generation is deterministic.
//
// **Determinism is what the consistency check above depends on**: if each run produced
// different bytes (for example an unsorted map somewhere), that check would always fail, and the
// failure would say "the table is stale", which does not point at the real cause at all.
func TestGenerateIsDeterministic(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	// Run three times: two runs might happen to hit the same map iteration order.
	for i := 2; i <= 3; i++ {
		again, err := Generate()
		if err != nil {
			t.Fatalf("第 %d 次生成失败：%v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("第 %d 次生成的结果与第一次不同——某处 map 没有排序输出。\n"+
				"这会让 TestGeneratedFileIsUpToDate 永远失败，"+
				"且报错指向「表过期」而不是真实原因。", i)
		}
	}
}

// TestGeneratedFileWarnsAgainstHandEditing checks that the top of the output says "do not edit
// by hand".
//
// Without that, the next person sees an ordinary .ts file and edits it, then gets stopped by the
// consistency test, and by then their edits are wasted. The repo's other two generated files
// (modelIconPaths.ts / brandPaths.ts) use the same phrase, so this asserts that existing wording.
func TestGeneratedFileWarnsAgainstHandEditing(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	s := string(generated)
	if !strings.Contains(s, "Do not edit it by hand") {
		t.Error("产物顶部没有「不要手改」的警示")
	}
	if !strings.Contains(s, "go run ./cmd/gen-authz-capabilities") {
		t.Error("产物顶部没有写重新生成的命令")
	}
}

// TestGeneratedFileHasNoUnicodeEscapes pins that Chinese text is not escaped as \uXXXX.
//
// # This is not pedantry
//
// Go's `%q` / strconv.Quote only guarantee ASCII safety and turn Chinese into `\u7a7a\u95f4`.
// Such output **compiles, runs, and even displays correctly in the UI**. The only thing broken is
// "people can read it", and this file is full of explanation text and comments meant for people.
//
// So it is a silent regression: if someone swaps tsString for %q one day, nothing fails.
func TestGeneratedFileHasNoUnicodeEscapes(t *testing.T) {
	generated, err := Generate()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if bytes.Contains(generated, []byte(`\u`)) {
		t.Error("产物里出现了 \\u 转义——中文被 strconv.Quote 之类转义了。\n" +
			"那样的文件能编译能跑，但没人读得懂，而它全是给人读的说明文案。\n" +
			"渲染字符串要用 authzdoc.tsString，不要用 fmt 的引号动词。")
	}
	// Positive assertion: there really is Chinese in it. The check above only proves "no
	// escapes", and an empty file satisfies it too.
	if !bytes.Contains(generated, []byte("权限")) {
		t.Error("产物里没有中文文案，可能渲染逻辑整个坏了")
	}
}

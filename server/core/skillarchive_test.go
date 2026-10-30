// Tests for the "container downloads the skill package directly" path.
package core

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

// seedSkillWithNamedFiles puts a given set of attachments into the seed skill (bytes go to
// object storage).
//
// Same approach as seedSkillWithFiles in skillark_test.go, except the caller supplies the
// files: several tests in this file need precise control over "does the package contain the
// user's own SKILL.md".
func seedSkillWithNamedFiles(
	t *testing.T, svc *Service, store *coremem.MemStore, files map[string][]byte,
) apimodel.Skill {
	t.Helper()
	ctx := context.Background()
	skills, err := store.ListSkills(ctx, seedWS)
	if err != nil || len(skills) == 0 {
		t.Fatalf("种子里应当有技能：%v", err)
	}
	id := skills[0].ID

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	stored := make([]apimodel.SkillFile, 0, len(files))
	meta := make([]apimodel.SkillFile, 0, len(files))
	for _, p := range paths {
		stored = append(stored, apimodel.SkillFile{Path: p, Content: files[p]})
		meta = append(meta, apimodel.SkillFile{Path: p, Size: len(files[p])})
	}
	putSkillFilesViaBlobs(t, ctx, svc, id, stored)

	// The metadata must also be recorded in the doc, otherwise skillinject cannot see it when
	// checking "are there attachments".
	sk, _ := store.GetSkill(ctx, id)
	sk.Files = meta
	saved, err := store.SaveSkill(ctx, sk)
	if err != nil {
		t.Fatalf("存技能失败：%v", err)
	}
	return saved
}

// signingBlobStore is a BlobStore stand-in that **can sign links**.
//
// ⚠️ **It must be a separate type**: ensureSkillArchive uses a type assertion to check "does
// this BlobStore support signing links", and the plain fakeBlobStore lacks those two methods.
// With it the archive path would be a no-op, so every assertion would be testing the "fall
// back to base64" path: **all tests green while not one line of the new code ran**.
type signingBlobStore struct {
	*fakeBlobStore
	// When signErr is non-nil, signing fails; used to check "fall back when signing fails".
	signErr error
}

func newSigningBlobStore() *signingBlobStore {
	return &signingBlobStore{fakeBlobStore: newFakeBlobStore()}
}

func (s *signingBlobStore) PresignedGetURL(key string, ttl time.Duration, reader LinkReader) (string, error) {
	if s.signErr != nil {
		return "", s.signErr
	}
	// The host changes with the reader, so tests can assert "the link was signed for the right party".
	host := "sandbox.example.test"
	if reader == LinkForPublic {
		host = "public.example.test"
	}
	return fmt.Sprintf("https://%s/obj/%s?sig=fake&ttl=%d", host, key, int(ttl.Seconds())), nil
}

func (s *signingBlobStore) Size(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return 0, ErrBlobNotFound
	}
	return int64(len(data)), nil
}

// TestSkillsForUsesArchiveAndKeepsUserSkillMD: a skill with attachments must use the archive,
// and **the fallback SKILL.md must be the user's own**.
//
// The second half was found in self-review: without it, when the download succeeds the
// container has **the one from the package** (uploaded by the user), but when the download
// fails and falls back it becomes **the one we built from Instructions**. The two may differ,
// so "the same skill has SKILL.md A sometimes and B other times in the container", and the
// switch condition is "did the download fail". Nobody would think to look there.
func TestSkillsForUsesArchiveAndKeepsUserSkillMD(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	blobs := newSigningBlobStore()
	svc.blobs = blobs

	const userMD = "# 用户自己那份 SKILL.md\n带 front-matter 之类\n"
	sk := seedSkillWithNamedFiles(t, svc, store, map[string][]byte{
		"SKILL.md":        []byte(userMD),
		"references/x.md": []byte("参考资料"),
		"scripts/init.sh": []byte("echo hi"),
	})
	bindSkills(t, store, seedChatAgent, sk.ID)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	bundles, err := NewSkillFileLoader(svc, nil).SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("SkillsFor 失败：%v", err)
	}

	var got *struct {
		URL   string
		Bytes int
		Files int
		MD    string
	}
	for _, b := range bundles {
		if b.Name != sk.Name {
			continue
		}
		md := ""
		for _, f := range b.Files {
			if f.Path == "SKILL.md" {
				md = string(f.Content)
			}
		}
		got = &struct {
			URL   string
			Bytes int
			Files int
			MD    string
		}{b.ArchiveURL, b.ArchiveBytes, len(b.Files), md}
	}
	if got == nil {
		t.Fatal("没找到这个技能的 bundle")
	}

	// (1) Uses the archive.
	if got.URL == "" {
		t.Error("有附件的技能应当带 ArchiveURL——否则大包会塞不进 shell 命令行")
	}
	if got.Bytes <= 0 {
		t.Error("要带字节数，否则容器侧没法校验「下全了没有」")
	}
	// (2) The fallback carries only SKILL.md, not the attachments too (that would defeat the archive).
	if got.Files != 1 {
		t.Errorf("兜底文件应当只有 1 个 SKILL.md，实际 %d 个", got.Files)
	}
	// (3) The fallback copy must be **the user's**, not one we built.
	if got.MD != userMD {
		t.Errorf("兜底 SKILL.md 应当用用户那份，实际是：%q\n"+
			"不一致的话「下载成功」与「下载失败」两条路会给出不同的正文，"+
			"而切换条件是下载有没有失败——没人会往这上面查", got.MD)
	}
}

// TestSkillsForFallsBackWhenSigningFails: when a link cannot be signed it must **fall back to
// per-file injection**, and must not fail the whole SkillsFor.
//
// If the error were passed up, the provider would inject nothing at all, so **not a single
// skill directory would be created**, and a missing directory is exactly how the model
// decides "this skill does not exist". With the fallback, at least small skills still work.
func TestSkillsForFallsBackWhenSigningFails(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	blobs := newSigningBlobStore()
	blobs.signErr = fmt.Errorf("签名服务炸了")
	svc.blobs = blobs

	sk := seedSkillWithNamedFiles(t, svc, store, map[string][]byte{
		"references/x.md": []byte("参考资料"),
	})
	bindSkills(t, store, seedChatAgent, sk.ID)

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	bundles, err := NewSkillFileLoader(svc, nil).SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatalf("签名失败不该让 SkillsFor 整个失败（那样一个技能目录都建不出来）：%v", err)
	}

	for _, b := range bundles {
		if b.Name != sk.Name {
			continue
		}
		if b.ArchiveURL != "" {
			t.Error("签名失败了不该还带 URL")
		}
		// On fallback, attachments must go into Files one by one.
		var hasAttachment bool
		for _, f := range b.Files {
			if f.Path == "references/x.md" {
				hasAttachment = true
			}
		}
		if !hasAttachment {
			t.Error("回落时附件应当逐个塞进 Files 走 base64 老路")
		}
		return
	}
	t.Fatal("没找到这个技能的 bundle")
}

// TestSkillArchiveKeyIsContentAddressed: signing the same content twice must give the same
// object key (otherwise the cache is useless and it re-uploads every time).
func TestSkillArchiveKeyIsContentAddressed(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	signing := &signingBlobStore{fakeBlobStore: blobs}
	svc.blobs = signing

	sk := apimodel.Skill{
		ID: "skill_x", WorkspaceID: seedWS, Name: "demo",
		Description: "d", Instructions: "正文",
	}
	files := []apimodel.SkillFile{{Path: "a.md", Content: []byte("内容")}}

	first, err := svc.ensureSkillArchive(ctx, sk, files, LinkForSandbox)
	if err != nil {
		t.Fatalf("第一次失败：%v", err)
	}
	nPuts := len(blobs.putKeys)

	second, err := svc.ensureSkillArchive(ctx, sk, files, LinkForSandbox)
	if err != nil {
		t.Fatalf("第二次失败：%v", err)
	}
	if first.Bytes != second.Bytes {
		t.Errorf("同一份内容两次算出的大小不同：%d vs %d", first.Bytes, second.Bytes)
	}
	// The second time should hit the cache and not Put again.
	if len(blobs.putKeys) != nPuts {
		t.Errorf("同一份内容不该重复上传：第一次 %d 个对象，第二次变成 %d 个",
			nPuts, len(blobs.putKeys))
	}
	// When the content changes, the key must change.
	changed, err := svc.ensureSkillArchive(ctx, sk,
		[]apimodel.SkillFile{{Path: "a.md", Content: []byte("改了")}}, LinkForSandbox)
	if err != nil {
		t.Fatalf("第三次失败：%v", err)
	}
	if strings.Contains(changed.URL, keyOf(first.URL)) {
		t.Error("内容变了 object key 必须跟着变，否则容器会下到旧包")
	}
}

// keyOf extracts the key part from a fake URL, only for the assertion above.
func keyOf(url string) string {
	const p = "/obj/"
	i := strings.Index(url, p)
	if i < 0 {
		return url
	}
	rest := url[i+len(p):]
	if j := strings.IndexByte(rest, '?'); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestLargeSkillStillGetsContainerArchive pins down "a skill over Ark's limit still gets a
// container package".
//
// # What this guards is "raising the limit to 100 MiB" itself
//
// ⚠️ **This bug really happened, and it wiped out a whole round of work raising the limit.**
// ensureSkillArchive used to call BuildSkillZip (which decides degradation by **Ark's**
// limit) and returned an error when it saw Degraded. The intent, "the container should not
// use a degraded package", was right, but it used the wrong function: so any skill over Ark's
// limit got no container package and fell back to base64 per-file injection.
//
// And base64 is exactly what cannot fit large packages (estimated from measurements: 60 MiB
// takes 853 commands, about 12.8 minutes; 100 MiB takes 21 minutes). Net effect: **"raising
// the limit to 100 MiB" did nothing at all for anything above 25 MiB**: those skills could
// neither go to Ark (degraded, correct) nor to the container (fell back to base64, sure to
// time out), and the only symptom was a Warn saying "fell back to per-file injection".
//
// It tests three sizes; 30/60 MiB are both above Ark's limit and within ours.
func TestLargeSkillStillGetsContainerArchive(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	svc.blobs = &signingBlobStore{fakeBlobStore: blobs}

	sk := apimodel.Skill{ID: "s_big", WorkspaceID: seedWS, Name: "big", Instructions: "正文"}
	for _, mb := range []int{8, 30, 60} {
		// Incompressible content: it stays large after compression, so it really hits the limit.
		body := make([]byte, mb<<20)
		for i := range body {
			body[i] = byte(i * 7919 % 251)
		}
		files := []apimodel.SkillFile{{Path: fmt.Sprintf("data-%d.bin", mb), Content: body}}

		arch, err := svc.ensureSkillArchive(ctx, sk, files, LinkForSandbox)
		if err != nil {
			t.Errorf("%d MiB 的技能应当拿得到容器包（容器侧没有方舟那些上限）：%v", mb, err)
			continue
		}
		if arch == nil {
			t.Errorf("%d MiB 的技能拿到了 nil——那会让 skillinject 回落到 base64，"+
				"而 base64 塞不下这么大的包", mb)
			continue
		}
		if arch.Bytes == 0 {
			t.Errorf("%d MiB 的技能包字节数是 0", mb)
		}
	}
}

// TestArkPathStillDegradesWhileContainerDoesNot is the **negative control** for the one above.
//
// Without it, "make BuildSkillZip never degrade too" would also turn the one above green, and
// that would send oversized packages to Ark for real. Ark replies with an English
// InvalidParameter, which shows up as "this agent cannot start a conversation".
//
// The two consumers must use **separate** rules: Ark degrades, the container does not.
func TestArkPathStillDegradesWhileContainerDoesNot(t *testing.T) {
	sk := apimodel.Skill{ID: "s", WorkspaceID: seedWS, Name: "big", Instructions: "正文"}
	body := make([]byte, 60<<20)
	for i := range body {
		body[i] = byte(i * 7919 % 251)
	}
	files := []apimodel.SkillFile{{Path: "data.bin", Content: body}}

	// Ark side: must degrade to SKILL.md only.
	arkPkg, err := BuildSkillZip(sk, files)
	if err != nil {
		t.Fatalf("方舟侧打包失败：%v", err)
	}
	if !arkPkg.Degraded {
		t.Error("60 MiB 超方舟上限，方舟侧必须降级——" +
			"不降的话方舟回英文 InvalidParameter，表现成「这个 agent 开不了对话」")
	}

	// Container side: must be the full package.
	full, err := BuildFullSkillZip(sk, files)
	if err != nil {
		t.Fatalf("容器侧打包失败：%v", err)
	}
	if len(full) <= len(arkPkg.Zip) {
		t.Errorf("容器包（%d 字节）应当明显大于降级后的方舟包（%d 字节）——"+
			"一样大说明容器包也被降级了", len(full), len(arkPkg.Zip))
	}
	// The attachment must really be in it.
	if !bytes.Contains(full, []byte("data.bin")) {
		t.Error("容器包里必须有附件，否则「容器里有完整技能」整个落空")
	}
}

// TestWarmMakesInjectionHitCache pins down "after warming, injection no longer packs and
// uploads".
//
// # What this tests is "the user does not feel the slowness" itself
//
// Pack + upload is the only expensive step in the whole chain, and it needs no container at
// all. Warming moves it to the moment **the conversation is created** (in `ensureArkSkill`,
// which has all attachment bytes at hand), while the container is only created when the user
// first really uses a tool. There are often several rounds of plain-text conversation in
// between, so this time is invisible to the user.
//
// The check is **upload count**, not elapsed time: timing is flaky in unit tests, while
// "was it uploaded again" is certain.
func TestWarmMakesInjectionHitCache(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	svc.blobs = &signingBlobStore{fakeBlobStore: blobs}

	sk := apimodel.Skill{ID: "s_warm", WorkspaceID: seedWS, Name: "warm", Instructions: "正文"}
	files := []apimodel.SkillFile{{Path: "a.md", Content: []byte("内容")}}

	// Warm (what happens when the conversation is created).
	svc.warmSkillArchive(ctx, sk, files)
	afterWarm := len(blobs.putKeys)
	if afterWarm == 0 {
		t.Fatal("预热应当把包传进对象存储")
	}

	// Inject (what happens when the container is created): should hit the cache and not upload again.
	arch, err := svc.ensureSkillArchive(ctx, sk, files, LinkForSandbox)
	if err != nil {
		t.Fatalf("注入时取包失败：%v", err)
	}
	if arch == nil || arch.URL == "" {
		t.Fatal("注入时应当拿到已签名的 URL")
	}
	if len(blobs.putKeys) != afterWarm {
		t.Errorf("注入时又传了一次（预热后 %d 个对象，注入后 %d 个）——预热没生效，"+
			"那 100 MiB 的打包上传仍然压在用户的首个工具调用上",
			afterWarm, len(blobs.putKeys))
	}
}

// TestWarmSkipsSkillsWithoutFiles: skills with zero attachments should not be warmed. They use
// base64 and need no archive at all.
//
// Without this, "always warm" would also turn the one above green, and that would pack and
// upload to object storage for nothing for every hand-written skill and the three built-in
// skills.
func TestWarmSkipsSkillsWithoutFiles(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	svc.blobs = &signingBlobStore{fakeBlobStore: blobs}

	sk := apimodel.Skill{ID: "s_plain", WorkspaceID: seedWS, Name: "plain", Instructions: "正文"}
	svc.warmSkillArchive(ctx, sk, nil)

	if len(blobs.putKeys) != 0 {
		t.Errorf("零附件的技能不该预热（它们走 base64），实际传了 %d 个对象",
			len(blobs.putKeys))
	}
}

// TestWarmFailureIsHarmless: a warming failure must not affect anything; it is purely an
// optimization.
func TestWarmFailureIsHarmless(t *testing.T) {
	ctx := actorCtx(t)
	svc, blobs := newServiceWithBlobs(t)
	blobs.putErr = fmt.Errorf("对象存储炸了")
	svc.blobs = &signingBlobStore{fakeBlobStore: blobs}

	sk := apimodel.Skill{ID: "s", WorkspaceID: seedWS, Name: "x", Instructions: "正文"}
	// No panic, no error (the function has no return value anyway). This is a behavior promise.
	svc.warmSkillArchive(ctx, sk, []apimodel.SkillFile{{Path: "a.md", Content: []byte("x")}})
}

// TestSkillArchiveLinkSignedForRightReader: ⭐ the skill package link must be signed for the
// right party: the session container (SkillsFor) uses sandboxEndpoint, the user's own machine
// (HostSkillsFor) uses publicEndpoint.
//
// Signing it wrong means the other side gets 403 on download and silently falls back to
// writing only SKILL.md (all attachments lost), with only a Warn.
func TestSkillArchiveLinkSignedForRightReader(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	svc.blobs = newSigningBlobStore()
	sk := seedSkillWithNamedFiles(t, svc, store, map[string][]byte{
		"SKILL.md": []byte("# x\n"), "a.md": []byte("附件"),
	})
	bindSkills(t, store, seedChatAgent, sk.ID)
	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatal(err)
	}
	loader := NewSkillFileLoader(svc, nil)

	box, err := loader.SkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	host, err := loader.HostSkillsFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var boxURL, hostURL string
	for _, b := range box {
		if b.Name == sk.Name {
			boxURL = b.ArchiveURL
		}
	}
	for _, b := range host {
		if b.Name == sk.Name {
			hostURL = b.ArchiveURL
		}
	}
	if !strings.HasPrefix(boxURL, "https://sandbox.example.test/") {
		t.Errorf("会话容器的链接应签给 sandbox 地址：%q", boxURL)
	}
	if !strings.HasPrefix(hostURL, "https://public.example.test/") {
		t.Errorf("用户机器的链接应签给 public 地址：%q", hostURL)
	}
}

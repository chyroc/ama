// Integration tests for storing Ark credentials encrypted.
//
// Unit tests (shared/infra/cryptobox) pin the encryptor itself; this file pins the **wiring**, that is,
// "the plaintext really did not land in the database" and "existing plaintext rows can still be read". Both are required:
// no matter how right the encryptor is, if the Store forgets to call it the database still holds plaintext, and every functional test stays green
// (the key can be stored, read and used; it just sits in the database as plaintext).
//
// When the database is unreachable the whole group calls t.Skip, for the same reason as store_pg_test.go.

package coredao

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/cryptobox"
)

// newCryptoTestKey generates a base64 key for tests.
func newCryptoTestKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, cryptobox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// pgStoreWithCrypto builds a PGStore with an encryptor (skips if the database is unreachable).
func pgStoreWithCrypto(t *testing.T, key string) *PGStore {
	t.Helper()
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	box, err := cryptobox.New(key)
	if err != nil {
		t.Fatalf("建加密器失败: %v", err)
	}
	ctx := context.Background()
	s, err := NewPGStore(ctx, pgEnv.client, WithCrypto(box))
	if err != nil {
		t.Fatalf("建 PGStore 失败: %v", err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	return s
}

// The plaintext must never appear in that database column; that is the whole meaning of "stored encrypted".
//
// ⚠️ The assertion must **query the database directly**. Checking with the Store's read methods cannot catch it: they decrypt, so whether
// the database holds plaintext or ciphertext, the read result is right. That is exactly why "forgot to encrypt" passes every test with zero errors.
func TestGlobalArkSecretIsEncryptedAtRest(t *testing.T) {
	s := pgStoreWithCrypto(t, newCryptoTestKey(t))
	ctx := context.Background()

	const email = "crypto-at-rest@example.com"
	const plain = "ark-plaintext-must-not-reach-disk-0001"

	if err := s.PutGlobalArkSecret(ctx, apimodel.GlobalArkSecret{
		Email:        email,
		APIKey:       plain,
		ApiKeyMasked: persistmodel.MaskSecret(plain),
		Region:       apimodel.RegionCNBeijing,
		Check:        apimodel.CheckUnchecked,
		UpdatedAt:    apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("存账号级凭证失败: %v", err)
	}

	var stored string
	if err := s.client.QueryRow(ctx,
		`select api_key from global_ark_secrets where email = $1`, email).Scan(&stored); err != nil {
		t.Fatalf("直查库失败: %v", err)
	}
	if strings.Contains(stored, plain) {
		t.Error("库里出现了明文 key —— 加密没生效")
	}
	if !strings.HasPrefix(stored, cryptobox.Prefix) {
		t.Errorf("库里那一列要带 %q 前缀，实际 %q", cryptobox.Prefix, stored)
	}

	// Must read back as plaintext, otherwise calling Ark with it gives 401.
	got, err := s.GetGlobalArkSecret(ctx, email)
	if err != nil {
		t.Fatalf("读账号级凭证失败: %v", err)
	}
	if got.APIKey != plain {
		t.Errorf("读回的明文不对：想要 %q，得到 %q", plain, got.APIKey)
	}
}

// The workspace-level key is in doc jsonb, and must not be stored in plaintext either.
//
// Tested separately instead of merged with the account level: the two are stored differently (real column vs a field inside jsonb),
// and encryption hooks in at different points (one on the SQL parameter, one before marshal). Testing only one side,
// a missing hook on the other side would go completely undetected.
func TestArkSecretIsEncryptedAtRest(t *testing.T) {
	s := pgStoreWithCrypto(t, newCryptoTestKey(t))
	ctx := context.Background()

	const ws = "ws-crypto-at-rest"
	const plain = "ark-workspace-plaintext-0002"

	if err := s.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID:  ws,
			ApiKeyMasked: persistmodel.MaskSecret(plain),
			Region:       apimodel.RegionCNBeijing,
			Check:        apimodel.CheckUnchecked,
			Source:       apimodel.SourceOverride,
			UpdatedAt:    apimodel.NowISO(),
		},
		APIKey: plain,
	}); err != nil {
		t.Fatalf("存空间级凭证失败: %v", err)
	}

	var doc string
	if err := s.client.QueryRow(ctx,
		`select doc::text from ark_secrets where workspace_id = $1`, ws).Scan(&doc); err != nil {
		t.Fatalf("直查库失败: %v", err)
	}
	if strings.Contains(doc, plain) {
		t.Error("doc 里出现了明文 key —— 加密没生效")
	}
	if !strings.Contains(doc, cryptobox.Prefix) {
		t.Errorf("doc 里应有 %q 前缀的密文，实际 %s", cryptobox.Prefix, doc)
	}

	got, err := s.GetArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("读空间级凭证失败: %v", err)
	}
	if got.APIKey != plain {
		t.Errorf("读回的明文不对：想要 %q，得到 %q", plain, got.APIKey)
	}
}

// PutArkSecret must not corrupt the caller's plaintext copy.
//
// Encryption happens in the Store, and after saving the Service often uses the same struct to call Ark
// (VerifyArkCredential does). If encryption wrote back into the argument, the key would be sent as ciphertext,
// Ark would return 401 "invalid API Key", and the real cause would be using our own ciphertext as plaintext.
func TestPutArkSecretDoesNotMutateCallerPlaintext(t *testing.T) {
	s := pgStoreWithCrypto(t, newCryptoTestKey(t))
	ctx := context.Background()

	const plain = "ark-caller-copy-must-stay-plain-0003"
	sec := apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: "ws-no-mutate",
			Region:      apimodel.RegionCNBeijing,
			Check:       apimodel.CheckUnchecked,
			Source:      apimodel.SourceOverride,
		},
		APIKey: plain,
	}
	if err := s.PutArkSecret(ctx, sec); err != nil {
		t.Fatalf("存失败: %v", err)
	}
	if sec.APIKey != plain {
		t.Errorf("入参被改成了 %q，调用方手上那份明文不能动", sec.APIKey)
	}
}

// Existing plaintext rows must remain readable; this is the whole basis for "no migration needed to ship".
//
// It is built by **bypassing the Store and writing a plaintext row directly**, simulating data in the database before encryption shipped.
// If this goes red, every configured Ark credential breaks at the moment of upgrade, and the error would say "cannot decrypt the credential".
func TestLegacyPlaintextRowsStillReadable(t *testing.T) {
	s := pgStoreWithCrypto(t, newCryptoTestKey(t))
	ctx := context.Background()

	const email = "legacy-plain@example.com"
	const legacy = "ark-written-before-encryption-existed"

	// Write plaintext directly, simulating an existing row from before encryption shipped.
	if _, err := s.client.Exec(ctx,
		`insert into global_ark_secrets
		   (email, api_key, api_key_masked, region, check_status, checked_at, check_error, updated_at)
		 values ($1, $2, $3, 'cn-beijing', 'unchecked', '', '', '')`,
		email, legacy, persistmodel.MaskSecret(legacy)); err != nil {
		t.Fatalf("插存量明文行失败: %v", err)
	}

	got, err := s.GetGlobalArkSecret(ctx, email)
	if err != nil {
		t.Fatalf("读存量明文行失败: %v", err)
	}
	if got.APIKey != legacy {
		t.Errorf("存量明文要原样读出：想要 %q，得到 %q", legacy, got.APIKey)
	}
}

// An instance without a configured key must error on a ciphertext row, not return the ciphertext as the key.
//
// This is a real case with multiple instances: old and new instances coexist during a rolling deploy, or someone forgot to configure the key on one instance.
// Returning ciphertext would send it to Ark, the 401 would say "invalid credential", and the real cause is "this instance is missing the key".
func TestStoreWithoutKeyRefusesCiphertextRow(t *testing.T) {
	key := newCryptoTestKey(t)
	enc := pgStoreWithCrypto(t, key)
	ctx := context.Background()

	const email = "ciphertext-no-key@example.com"
	if err := enc.PutGlobalArkSecret(ctx, apimodel.GlobalArkSecret{
		Email:     email,
		APIKey:    "ark-sealed-with-a-key-0004",
		Region:    apimodel.RegionCNBeijing,
		Check:     apimodel.CheckUnchecked,
		UpdatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("存失败: %v", err)
	}

	// Same database, but this Store has no key (no Clear; keep the row above).
	plainStore, err := NewPGStore(ctx, pgEnv.client)
	if err != nil {
		t.Fatalf("建无密钥 PGStore 失败: %v", err)
	}
	got, err := plainStore.GetGlobalArkSecret(ctx, email)
	if err == nil {
		t.Fatalf("没有密钥读密文行应当报错，却读出了 %q", got.APIKey)
	}
	if strings.Contains(err.Error(), "ark-sealed") {
		t.Error("报错信息里不能带密文/明文内容")
	}
}

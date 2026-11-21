package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/filebytes"
)

const DefaultMigrateBlobLimit = 50

// BlobMigrationStore separates table access from migration policy. AttachFileToRow must
// insert the file record and update the business row in one transaction, with an
// optimistic "file_id is still null" condition that detects another concurrent migration.
type BlobMigrationStore interface {
	ListUnmigratedBlobs(context.Context, persistmodel.BlobMigrationTable, int) ([]persistmodel.BlobRowKey, error)
	GetBlobForMigration(context.Context, persistmodel.BlobMigrationTable, persistmodel.BlobRowKey) (persistmodel.BlobPayload, error)
	AttachFileToRow(context.Context, persistmodel.BlobMigrationTable, persistmodel.BlobRowKey, persistmodel.FileRecord) error
}

// BlobMigrationHooks supplies only explicitly enumerated tables. Authorization belongs to
// the caller; this service must not become a public unauthenticated migration endpoint.
type BlobMigrationHooks struct {
	Store  BlobMigrationStore
	Blobs  BlobStore
	Tables []persistmodel.BlobMigrationTable
	Prefix func(persistmodel.BlobMigrationTable) (string, error)
	NewID  func() string
	NowISO func() string
	// MissingAuthor labels rows that predate author metadata, instead of leaving an
	// ambiguous blank value in the files table.
	MissingAuthor string
	Logger        *slog.Logger
}

// BlobMigrationService moves legacy database bytes to object storage in resumable batches.
type BlobMigrationService struct{ hooks BlobMigrationHooks }

func NewBlobMigrationService(hooks BlobMigrationHooks) *BlobMigrationService {
	return &BlobMigrationService{hooks: hooks}
}

// Migrate performs no remote writes in dry-run. Apply without an explicit limit uses a
// bounded batch so a request has a known result rather than an ambiguous gateway timeout.
func (s *BlobMigrationService) Migrate(
	ctx context.Context, actor string, apply bool, limit int,
) (apimodel.MigrateBlobReport, error) {
	var zero apimodel.MigrateBlobReport
	if s.hooks.Store == nil || s.hooks.Blobs == nil || s.hooks.Prefix == nil || s.hooks.NewID == nil {
		return zero, fmt.Errorf("%w: 没有完整配置对象存储与迁移存储，没法迁移", ErrPrecondition)
	}
	if limit < 0 {
		return zero, fmt.Errorf("%w: limit 不能是负数", ErrInvalidArgument)
	}
	if apply && limit == 0 {
		limit = DefaultMigrateBlobLimit
	}
	logger := s.hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "开始存量字节迁移", "actor", actor, "apply", apply, "limit", limit)
	report := apimodel.MigrateBlobReport{
		Apply: apply, Limit: limit, Bucket: s.hooks.Blobs.Bucket(),
		Tables: make([]apimodel.MigrateBlobTableReport, 0, len(s.hooks.Tables)),
		Items:  make([]apimodel.MigrateBlobItem, 0),
	}
	for _, table := range s.hooks.Tables {
		keys, err := s.hooks.Store.ListUnmigratedBlobs(ctx, table, limit)
		if err != nil {
			return zero, fmt.Errorf("列 %s 的待迁移记录失败: %w", table, err)
		}
		tableReport := apimodel.MigrateBlobTableReport{Table: string(table), Pending: len(keys)}
		if !apply {
			report.Tables = append(report.Tables, tableReport)
			report.TotalPending += tableReport.Pending
			continue
		}
		for _, key := range keys {
			item := s.migrateOne(ctx, table, key, actor, logger)
			report.Items = append(report.Items, item)
			if item.OK {
				tableReport.Migrated++
			} else {
				tableReport.Failed++
			}
		}
		report.Tables = append(report.Tables, tableReport)
		report.TotalPending += tableReport.Pending
		report.TotalMigrated += tableReport.Migrated
		report.TotalFailed += tableReport.Failed
	}
	logger.InfoContext(ctx, "存量字节迁移结束", "actor", actor, "apply", apply,
		"pending", report.TotalPending, "migrated", report.TotalMigrated, "failed", report.TotalFailed)
	return report, nil
}

// migrateOne verifies the object before attaching it to a business row. A failed row is
// reported and skipped, not marked migrated. If the final database write fails, deleting
// the object with another remote call would add a second failure; report the orphan instead.
func (s *BlobMigrationService) migrateOne(
	ctx context.Context, table persistmodel.BlobMigrationTable,
	key persistmodel.BlobRowKey, actor string, logger *slog.Logger,
) apimodel.MigrateBlobItem {
	item := apimodel.MigrateBlobItem{Table: string(table), Key: key.String()}
	fail := func(err error) apimodel.MigrateBlobItem {
		item.Error = err.Error()
		logger.WarnContext(ctx, "迁移一条记录失败，跳过", "table", table, "key", key.String(), "err", err)
		return item
	}
	payload, err := s.hooks.Store.GetBlobForMigration(ctx, table, key)
	if err != nil {
		return fail(err)
	}
	if len(payload.Content) == 0 {
		return fail(errors.New("content 是空的，跳过（这条数据本身有问题）"))
	}
	author := strings.TrimSpace(payload.Author)
	if author == "" {
		author = s.hooks.MissingAuthor
	}
	createdAt := strings.TrimSpace(payload.CreatedAt)
	if createdAt == "" {
		if s.hooks.NowISO == nil {
			return fail(errors.New("存量记录缺少创建时间，且没有装配时间生成器"))
		}
		createdAt = s.hooks.NowISO()
	}
	prefix, err := s.hooks.Prefix(table)
	if err != nil {
		return fail(err)
	}
	fileID := s.hooks.NewID()
	objectKey := filebytes.ObjectKey(prefix, fileID, createdAt)
	if err := s.hooks.Blobs.Put(ctx, objectKey, payload.Content, payload.ContentType); err != nil {
		return fail(fmt.Errorf("写对象存储失败: %w", err))
	}
	got, err := s.hooks.Blobs.Get(ctx, objectKey)
	if err != nil {
		return fail(fmt.Errorf("回读校验失败: %w", err))
	}
	if filebytes.ContentSHA256(got) != filebytes.ContentSHA256(payload.Content) {
		return fail(fmt.Errorf("回读的字节与原始不一致（%d vs %d 字节），不改库", len(got), len(payload.Content)))
	}
	if err := s.hooks.Store.AttachFileToRow(ctx, table, key, persistmodel.FileRecord{
		ID: fileID, Bucket: s.hooks.Blobs.Bucket(), ObjectKey: objectKey,
		Filename: payload.Filename, ContentType: payload.ContentType,
		Size: len(payload.Content), ContentSHA256: filebytes.ContentSHA256(payload.Content),
		Author: author, CreatedAt: createdAt,
	}); err != nil {
		return fail(fmt.Errorf("落库失败（对象已写，留下孤儿）: %w", err))
	}
	item.OK = true
	item.Bytes = len(payload.Content)
	item.FileID = fileID
	return item
}

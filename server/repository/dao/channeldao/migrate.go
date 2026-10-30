// Package channeldao is the PostgreSQL storage of server/channel and of the channels built on it
// (WeChat today). Feishu keeps its own package (feishudao) and tables.
package channeldao

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/repository/schema"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// SQLSTATEs that mean "another instance created it first" when several instances start at once.
const (
	sqlStateDuplicateTable  = "42P07"
	sqlStateDuplicateObject = "42710"
	sqlStateUniqueViolation = "23505"
)

// Migrate creates the channel tables (schema.ChannelPG) if they do not exist. Safe to run on every start.
func Migrate(ctx context.Context, client *pg.Client) error {
	if client == nil {
		return fmt.Errorf("channeldao: pg.Client 不能为 nil")
	}
	// DDL runs on a connection without statement_timeout; see pg.WithoutQueryTimeouts.
	return client.WithoutQueryTimeouts(ctx, func(ctx context.Context, exec pg.ExecFunc) error {
		for _, script := range schema.ChannelPG() {
			if _, err := exec(ctx, script.SQL); err != nil {
				state := pg.SQLState(err)
				if state != sqlStateDuplicateTable && state != sqlStateDuplicateObject && state != sqlStateUniqueViolation {
					return fmt.Errorf("建 channel 的表失败（%s）: %w", script.Name, err)
				}
			}
		}
		return nil
	})
}

//go:build integration

package repository

// designkit 集成测试的地基（CLAUDE.md 二·五 F 说的那份 harness）。
//
// 为什么是「抄」而不是 import：上游的 harness 在
// backend/internal/repository/integration_harness_test.go，是 _test.go 文件、
// 函数全部未导出，跨包根本 import 不到，只能照它的形态在这里放一份。
//
// 跟上游 harness 的三点差异（都是刻意的）：
//   - 只起 PostgreSQL 一个容器。designkit 仓储只依赖 *sql.DB（CLAUDE.md 二·五 A2），
//     不用 Redis、不用 ent，那两截不抄。
//   - 迁移不抄：ApplyMigrations 本身是上游导出的函数（migrations_runner.go），
//     直接调用。它应用的是全量嵌入迁移——上游 001~220 和我们的 9xxx 一起跑，
//     所以 9001~9003 在真 PostgreSQL 上天然被测到（这正是要这份 harness 的原因：
//     `pq: inconsistent types deduced` 这类错误只在真库的 PREPARE 阶段暴露，
//     假 repo 的单测永远碰不到）。
//   - 测试直接用共享的 integrationDB 加 t.Cleanup 删数据，不提供 testTx：
//     并发认领那类测试必须多连接同时写，包在单个事务里就测不到锁行为了。
//
// 跑法（Mac 上没有 Go 和 Docker，一律在 NAS 或 CI 上跑）：
//
//	cd backend && go test -tags=integration ./internal/designkit/repository/
//
// 本机没有 Docker 时自动跳过；CI（环境变量 CI 非空）里 Docker 缺失则直接判失败，
// 不允许「悄悄跳过」把集成测试变成永远绿灯的空转。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	upstreamrepo "github.com/Wei-Shaw/sub2api/internal/repository"

	_ "github.com/lib/pq"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// postgresImageTag 跟上游 harness 保持同一个镜像版本，
// 两套集成测试在 CI 上只需要拉一份镜像。
const postgresImageTag = "postgres:18.1-alpine3.23"

// integrationDB 是整个包的集成测试共用的连接池，TestMain 里初始化。
// 数据不自动回滚：每个测试用自己独立的 user_id / uid，并在 t.Cleanup 里删自己的行。
var integrationDB *sql.DB

func TestMain(m *testing.M) {
	ctx := context.Background()

	// 跟上游 harness 一致：统一 UTC，时间断言不受跑测试那台机器的时区影响。
	if err := timezone.Init("UTC"); err != nil {
		log.Printf("failed to init timezone: %v", err)
		os.Exit(1)
	}

	if !dockerIsAvailable(ctx) {
		// CI 上 Docker 必须在：这里失败要响，绝不能静默跳过（否则集成测试
		// 又变回「CI 那句 make test-integration 对我们是空跑」）。
		if os.Getenv("CI") != "" {
			log.Printf("docker is not available (CI=true); failing integration tests")
			os.Exit(1)
		}
		log.Printf("docker is not available; skipping designkit integration tests (start Docker to enable)")
		os.Exit(0)
	}

	pgContainer, err := tcpostgres.Run(
		ctx,
		selectPostgresImage(),
		tcpostgres.WithDatabase("sub2api_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Printf("failed to start postgres container: %v", err)
		os.Exit(1)
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	if err != nil {
		log.Printf("failed to get postgres dsn: %v", err)
		os.Exit(1)
	}

	integrationDB, err = openSQLWithRetry(ctx, dsn, 30*time.Second)
	if err != nil {
		log.Printf("failed to open sql db: %v", err)
		os.Exit(1)
	}

	// 全量迁移：上游 001~220 + 我们的 9001~9003 一起在真 PostgreSQL 上跑。
	// 这里失败 = 迁移本身有问题，整个包直接不跑。
	if err := upstreamrepo.ApplyMigrations(ctx, integrationDB); err != nil {
		log.Printf("failed to apply db migrations: %v", err)
		os.Exit(1)
	}

	code := m.Run()

	_ = integrationDB.Close()

	os.Exit(code)
}

func dockerIsAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}

// selectPostgresImage 支持用 SUB2API_TEST_POSTGRES_IMAGE 换镜像版本。
// 环境变量名跟上游 harness 用同一个：想验「最老的受支持 PostgreSQL」时，
// 一个变量同时管两套集成测试。
func selectPostgresImage() string {
	if override := strings.TrimSpace(os.Getenv("SUB2API_TEST_POSTGRES_IMAGE")); override != "" {
		return override
	}
	return postgresImageTag
}

func openSQLWithRetry(ctx context.Context, dsn string, timeout time.Duration) (*sql.DB, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}

		if err := pingWithTimeout(ctx, db, 2*time.Second); err != nil {
			lastErr = err
			_ = db.Close()
			time.Sleep(250 * time.Millisecond)
			continue
		}

		return db, nil
	}

	return nil, fmt.Errorf("db not ready after %s: %w", timeout, lastErr)
}

func pingWithTimeout(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return db.PingContext(pingCtx)
}

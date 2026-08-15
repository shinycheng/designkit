// Package db 为 designkit 模块提供一个独立的 PostgreSQL 连接池。
//
// 为什么不复用上游那一个：
// `server.registerRoutes`（backend/internal/server/router.go）能拿到的参数里
// 既没有 *sql.DB 也没有 *ent.Client。想拿到就得改 server/http.go 里
// ProvideRouter 的签名，而那会要求重新生成 backend/cmd/server/wire_gen.go
// ——那是已入库的上游文件，一改就是每次跟版的巨型冲突。
// 见 CLAUDE.md 第二·五节 A1。
//
// 代价是多一个连接池（所以下面对连接数做了收敛），换来零上游改动。
package db

import (
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	// PostgreSQL 驱动：注册 "postgres"。上游 internal/setup/setup.go 也是这么引的。
	_ "github.com/lib/pq"
)

const (
	// 下面三个值照抄上游 backend/internal/repository/db_pool.go 的同名常量。
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
	maxConfiguredConnAge   = 24 * time.Hour

	// 连接数上限刻意**不**照抄上游（上游默认 database.max_open_conns = 256，
	// 见 config.go 的 viper.SetDefault("database.max_open_conns", 256)）。
	// 理由：这是进程里的第二个池，两个池打的是同一个 PostgreSQL。
	// 群晖上跑的 PostgreSQL 默认 max_connections = 100，256 + 256 会直接把
	// 数据库连接打满，连上游网关一起拖死。
	// designkit 的并发上限设计值是 4（设计定型 第六节），16 条连接足够，
	// 仍然从 cfg 读值、只是压到这个上限以内。
	maxDesignkitOpenConns = 16
	maxDesignkitIdleConns = 4
)

// ErrNilConfig 传进来的配置为空。
var ErrNilConfig = errors.New("designkit db: config is nil")

// Pool 是 designkit 自己的数据库连接池。
// 拿到之后一律通过 DB() 取 *sql.DB 交给 repository 层用（repository 只依赖
// *sql.DB，完全不碰 ent，形态照抄上游 repository/batch_image_repo.go）。
type Pool struct {
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

type poolSettings struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
	connMaxIdleTime time.Duration
}

// Open 建立 designkit 的连接池。
//
// 注意：database/sql 的 Open 只解析 DSN、不会真的连数据库，所以这里返回 nil
// 不代表数据库是通的。真正的连通性由健康检查里的 SELECT 1 负责。
//
// DSN 直接复用上游 config.DatabaseConfig.DSNWithTimezone：它拼的是 libpq 的
// keyword/value 形式（host=… password=…），不是 URL 形式，所以密码里带
// @ / % / : 这些字符**不需要**转义，照抄上游即可。
func Open(cfg *config.Config) (*Pool, error) {
	if cfg == nil {
		return nil, ErrNilConfig
	}

	// 与上游 repository.InitEnt 用同一个 DSN 构造方式，保证时区行为一致。
	dsn := cfg.Database.DSNWithTimezone(cfg.Timezone)

	sqlDB, err := sql.Open("postgres", dsn)
	if err != nil {
		// 不把 dsn 放进错误里——里面有数据库密码。
		return nil, err
	}

	s := resolvePoolSettings(cfg)
	sqlDB.SetMaxOpenConns(s.maxOpenConns)
	sqlDB.SetMaxIdleConns(s.maxIdleConns)
	sqlDB.SetConnMaxLifetime(s.connMaxLifetime)
	sqlDB.SetConnMaxIdleTime(s.connMaxIdleTime)

	slog.Info("designkit 数据库连接池已配置",
		slog.Int("max_open", s.maxOpenConns),
		slog.Int("max_idle", s.maxIdleConns),
		slog.Duration("max_lifetime", s.connMaxLifetime),
		slog.Duration("max_idle_time", s.connMaxIdleTime),
	)

	return &Pool{db: sqlDB}, nil
}

// DB 返回底层的 *sql.DB。
func (p *Pool) DB() *sql.DB {
	if p == nil {
		return nil
	}
	return p.db
}

// Close 关闭连接池，可重复调用（第二次起返回第一次的结果）。
func (p *Pool) Close() error {
	if p == nil || p.db == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.closeErr = p.db.Close()
	})
	return p.closeErr
}

func resolvePoolSettings(cfg *config.Config) poolSettings {
	return poolSettings{
		maxOpenConns:    clampConns(cfg.Database.MaxOpenConns, maxDesignkitOpenConns),
		maxIdleConns:    clampConns(cfg.Database.MaxIdleConns, maxDesignkitIdleConns),
		connMaxLifetime: clampAge(cfg.Database.ConnMaxLifetimeMinutes, defaultConnMaxLifetime),
		connMaxIdleTime: clampAge(cfg.Database.ConnMaxIdleTimeMinutes, defaultConnMaxIdleTime),
	}
}

// clampConns 把配置值收敛到 [1, limit]。配置里是 0 或负数时用 limit
// （上游 Validate 会保证 max_open_conns > 0，这里只是不依赖它）。
func clampConns(configured, limit int) int {
	if configured <= 0 || configured > limit {
		return limit
	}
	return configured
}

// clampAge 照抄上游 repository.clampDBPoolDuration 的判定逻辑。
func clampAge(minutes int, fallback time.Duration) time.Duration {
	if minutes <= 0 || minutes > int(maxConfiguredConnAge/time.Minute) {
		return fallback
	}
	return time.Duration(minutes) * time.Minute
}

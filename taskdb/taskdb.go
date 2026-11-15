package taskdb

import (
	"errors"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	KindSearch   = "search"
	KindDownload = "download"

	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

type Task struct {
	ID         int64  `gorm:"primaryKey;autoIncrement;index:idx_tasks_status_id,priority:2"`
	Kind       string `gorm:"uniqueIndex:idx_tasks_kind_url,priority:1;not null"`
	URL        string `gorm:"uniqueIndex:idx_tasks_kind_url,priority:2;not null"`
	Params     string `gorm:"not null"`
	Status     string `gorm:"index:idx_tasks_status_id,priority:1;not null"`
	Attempts   int    `gorm:"not null"`
	LastError  string `gorm:"not null"`
	ResultPath string `gorm:"not null"`
	BytesTotal int64  `gorm:"not null"`
	BytesDone  int64  `gorm:"not null"`
	CreatedAt  int64  `gorm:"not null"`
	UpdatedAt  int64  `gorm:"not null"`
	FreshUntil int64  `gorm:"not null"`
}

func Open(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open("file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&Task{}); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func Submit(db *gorm.DB, kind, url, params string, freshUntil int64) (int64, error) {
	now := time.Now().Unix()
	var t Task
	err := db.Where("kind = ? AND url = ?", kind, url).First(&t).Error
	if err == nil {
		if (t.Status == StatusDone && t.FreshUntil > now) || t.Status == StatusPending || t.Status == StatusRunning {
			return t.ID, nil
		}
		if uerr := db.Model(&Task{}).Where("id = ?", t.ID).Updates(map[string]any{
			"status": StatusPending, "attempts": 0, "last_error": "", "result_path": "",
			"bytes_total": 0, "bytes_done": 0, "updated_at": now, "fresh_until": freshUntil,
		}).Error; uerr != nil {
			return 0, uerr
		}
		return t.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	t = Task{Kind: kind, URL: url, Params: params, Status: StatusPending, CreatedAt: now, UpdatedAt: now, FreshUntil: freshUntil}
	if cerr := db.Create(&t).Error; cerr != nil {
		var existing Task
		if rerr := db.Where("kind = ? AND url = ?", kind, url).First(&existing).Error; rerr == nil {
			return existing.ID, nil
		}
		return 0, cerr
	}
	return t.ID, nil
}

func Get(db *gorm.DB, id int64) (*Task, error) {
	var t Task
	if err := db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

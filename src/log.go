package git_pages

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"
)

var logc slogWithCtx

type slogWithCtx struct{}

func (l slogWithCtx) log(ctx context.Context, level slog.Level, msg string) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := slog.Default()
	if !logger.Enabled(ctx, level) {
		return
	}

	var pcs [1]uintptr
	// skip [runtime.Callers, this method, method calling this method]
	runtime.Callers(3, pcs[:])

	record := slog.NewRecord(time.Now(), level, strings.TrimRight(msg, "\n"), pcs[0])
	logger.Handler().Handle(ctx, record)
}

func (l slogWithCtx) Print(ctx context.Context, v ...any) {
	l.log(ctx, slog.LevelInfo, fmt.Sprint(v...))
}

func (l slogWithCtx) Printf(ctx context.Context, format string, v ...any) {
	l.log(ctx, slog.LevelInfo, fmt.Sprintf(format, v...))
}

func (l slogWithCtx) Println(ctx context.Context, v ...any) {
	l.log(ctx, slog.LevelInfo, fmt.Sprintln(v...))
}

func (l slogWithCtx) Fatalf(ctx context.Context, format string, v ...any) {
	l.log(ctx, slog.LevelError, fmt.Sprintf(format, v...))
	os.Exit(1)
}

func (l slogWithCtx) Fatalln(ctx context.Context, v ...any) {
	l.log(ctx, slog.LevelError, fmt.Sprintln(v...))
	os.Exit(1)
}

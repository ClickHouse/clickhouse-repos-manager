// Command cleanup deletes test data below ch-deploy-test/ in the test bucket.
//
//	go run ./test/cleanup [-stale 24h] [prefix...]
//	go run ./test/cleanup -new-prefix
//
// The bucket comes from CH_DEPLOY_TEST_ENDPOINT, CH_DEPLOY_TEST_BUCKET,
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ClickHouse/clickhouse-repos-manager/internal/storetest"
)

func main() {
	stale := flag.Duration("stale", 0, "also delete run prefixes older than this; 0 disables")
	newPrefix := flag.Bool("new-prefix", false, "print a fresh run prefix and exit")
	flag.Parse()
	if *newPrefix {
		fmt.Println(storetest.NewRunPrefix(time.Now()))
		return
	}
	if err := run(*stale, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup:", err)
		os.Exit(1)
	}
}

func run(stale time.Duration, prefixes []string) error {
	env, ok := storetest.FromEnv()
	if !ok {
		return fmt.Errorf("CH_DEPLOY_TEST_ENDPOINT and CH_DEPLOY_TEST_BUCKET are not set")
	}
	for _, p := range prefixes {
		if err := storetest.CheckRunPrefix(p); err != nil {
			return err
		}
	}
	ctx := context.Background()
	failed := false
	for _, p := range prefixes {
		n, err := storetest.Purge(ctx, env, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: %s: %d objects deleted, then: %v\n", p, n, err)
			failed = true
			continue
		}
		fmt.Printf("cleanup: %s: %d objects deleted\n", p, n)
	}
	if stale > 0 {
		purged, err := storetest.PurgeStale(ctx, env, stale, time.Now())
		for _, p := range purged {
			fmt.Printf("cleanup: %s: stale, deleted\n", p)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: stale: %v\n", err)
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("test data may be left in the bucket")
	}
	return nil
}

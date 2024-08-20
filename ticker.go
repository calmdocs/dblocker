package dblocker

import (
	"context"
	"fmt"
	"time"
)

func (s *Store) ticker(parentCtx context.Context, tag string) context.CancelFunc {
	ticker := time.NewTicker(2 * time.Second)
	ctx, cancel := context.WithCancel(parentCtx)
	go func() {
		defer ticker.Stop()

		startTime := time.Now()
		count := 0
		for {
			select {
			case <-s.Ctx.Done():
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Printf("dblocker ticker count (%d) duration (%v): %s \n", count, time.Since(startTime), tag)
				count++
			}
		}
	}()
	return cancel
}

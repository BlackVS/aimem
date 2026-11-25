// Command stuck stands in for an aimem process that does not end by
// itself, such as a sync waiting on a slow hub: whatever its arguments,
// it runs until it is ended.
package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}

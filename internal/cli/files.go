package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// File selection (DECISIONS D44). Users see and type file numbers from 1,
// as listed by 'denco files' and 'denco inspect'; the API uses indexes
// from 0.

const filesUsage = `usage:
  denco files <id>                     list the files with progress
  denco files <id> skip <files>...     don't download these files
  denco files <id> want <files>...     download these files again
  <files>: numbers from 'denco files', e.g. 3  2-5  1,4,7-9  all`

// cmdFiles lists a torrent's files, or changes which of them are wanted.
func cmdFiles(e *env, args []string) int {
	if len(args) == 1 {
		return listFiles(e, args[0])
	}
	if len(args) < 3 || (args[1] != "skip" && args[1] != "want") {
		fmt.Fprintln(e.stderr, filesUsage)
		return 2
	}
	ref, wanted, spec := args[0], args[1] == "want", args[2:]
	return withClient(e, func(ctx context.Context, c *ipc.Client) error {
		t, err := c.Get(ctx, ref)
		if err != nil {
			return err
		}
		if !t.HasMetadata {
			return fmt.Errorf("#%d has no file list yet: its metadata is still being fetched", t.ID)
		}
		idx, err := parseFileList(spec, t.Files)
		if err != nil {
			return err
		}
		files, err := c.SetFiles(ctx, ref, idx, wanted)
		if err != nil {
			return err
		}
		verb := "Skipping"
		if wanted {
			verb = "Wanting"
		}
		fmt.Fprintf(e.stdout, "%s %d file%s.\n\n", verb, len(idx), plural(len(idx)))
		if t, err = c.Get(ctx, ref); err == nil { // sizes and state after the change
			renderFiles(e.stdout, t, files)
		}
		return err
	})
}

func listFiles(e *env, ref string) int {
	return withClient(e, func(ctx context.Context, c *ipc.Client) error {
		t, err := c.Get(ctx, ref)
		if err != nil {
			return err
		}
		files, err := c.Files(ctx, ref)
		if err == nil {
			renderFiles(e.stdout, t, files)
		}
		return err
	})
}

// parseFileList reads file numbers as users type them — counted from 1,
// "3", "2-5", "1,4,7-9" or "all", across one or more arguments — and
// returns indexes from 0, sorted and without duplicates.
func parseFileList(args []string, n int) ([]int, error) {
	var out []int
	for _, arg := range args {
		for _, tok := range strings.Split(arg, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if tok == "all" {
				for i := 0; i < n; i++ {
					out = append(out, i)
				}
				continue
			}
			lo, hi, isRange := strings.Cut(tok, "-")
			a, err1 := strconv.Atoi(lo)
			b := a
			var err2 error
			if isRange {
				b, err2 = strconv.Atoi(hi)
			}
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("%q is not a file number or range (e.g. 3 or 2-5)", tok)
			}
			if a < 1 || b > n {
				return nil, fmt.Errorf("file %s doesn't exist: the torrent has files 1-%d", tok, n)
			}
			for i := a; i <= b; i++ {
				out = append(out, i-1)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no files given")
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// skipFromFlags turns `add -skip LIST` / `add -only LIST` into skipped
// file indexes for a torrent with n files.
func skipFromFlags(skip, only string, n int) ([]int, error) {
	switch {
	case skip != "" && only != "":
		return nil, fmt.Errorf("use -skip or -only, not both")
	case skip != "":
		return parseFileList([]string{skip}, n)
	case only != "":
		keep, err := parseFileList([]string{only}, n)
		if err != nil {
			return nil, err
		}
		var out []int
		for i := 0; i < n; i++ {
			if !slices.Contains(keep, i) {
				out = append(out, i)
			}
		}
		return out, nil
	}
	return nil, nil
}

// selectedSize describes a torrent's size, mentioning skipped files.
func selectedSize(wanted, total int64, skipped int) string {
	if skipped == 0 {
		return format.Bytes(total)
	}
	return fmt.Sprintf("%s selected of %s (%d file%s skipped)", format.Bytes(wanted), format.Bytes(total), skipped, plural(skipped))
}

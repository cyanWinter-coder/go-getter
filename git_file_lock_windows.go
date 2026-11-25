// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package getter

import "context"

// lockGitProcessFile currently relies on the in-process destination lock on
// Windows. GitGetter calls in the same process are therefore serialized.
func lockGitProcessFile(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build !unix && !windows

package getter

import "context"

// lockGitProcessFile falls back to the in-process destination lock on
// platforms without a platform-specific implementation.
func lockGitProcessFile(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

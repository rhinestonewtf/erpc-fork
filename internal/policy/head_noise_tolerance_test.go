package policy

import (
	"strconv"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// Fork patch RHI-8079 — see PATCH_LIST.md. The corroborated head may lead its
// partner only as far as the default policy tolerates lag; if the policy's
// threshold moves, the tolerance must move with it.
func TestCorroboratedHeadLeadTolerance_MatchesDefaultPolicyLagThreshold(t *testing.T) {
	want := "blockNumberLagAbove(" + strconv.FormatInt(common.CorroboratedHeadLeadTolerance, 10) + ")"
	assert.True(t, strings.Contains(DefaultPolicySource(), want),
		"default_policy.js must exclude on %s to match common.CorroboratedHeadLeadTolerance", want)
}

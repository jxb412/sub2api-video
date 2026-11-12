//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Every public account mutation must reject malformed retry settings before a
// repository write. Otherwise an import/bulk edit could bypass the form guard.
func TestAdminVideoExecutionPolicyRejectsInvalidWrites(t *testing.T) {
	for _, method := range []string{"create", "update", "extra", "bulk"} {
		t.Run(method, func(t *testing.T) {
			repo := &longContextBillingRepoStub{account: &Account{ID: 1, Platform: PlatformOpenAI}}
			svc := &adminServiceImpl{accountRepo: repo}
			extra := map[string]any{"video_execution_policy": map[string]any{"max_retries": -1}}
			var err error
			switch method {
			case "create":
				_, err = svc.CreateAccount(context.Background(), &CreateAccountInput{Extra: extra})
			case "update":
				_, err = svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: extra})
			case "extra":
				err = svc.UpdateAccountExtra(context.Background(), 1, extra)
			case "bulk":
				_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Extra: extra})
			}
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Nil(t, repo.createdAccount)
			require.Nil(t, repo.account.Extra)
			require.Zero(t, repo.updateExtraCalls)
			require.Zero(t, repo.bulkUpdateCalls)
		})
	}
}

func TestAdminVideoExecutionPolicyCreateAndReset(t *testing.T) {
	repo := &longContextBillingRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo}
	extra := map[string]any{"video_execution_policy": map[string]any{"retry_enabled": false, "max_retries": 0}}
	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name: "video", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test"}, Extra: extra, SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)
	require.NotNil(t, account.Extra["video_execution_policy"])
	account, err = svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{
		Extra: map[string]any{"video_execution_policy": nil},
	})
	require.NoError(t, err)
	require.Nil(t, account.Extra["video_execution_policy"])
}

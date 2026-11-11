//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestVideoOperationsRequireAuditableAdministrator(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	for name, call := range map[string]func(*gin.Context){"reconcile": h.ReconcileVideoTask, "retry": h.RetryVideoTask, "fail": h.FailVideoTask} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/videos/test/"+name, strings.NewReader(`{"reason":"test","actual_cost":1}`))
			c.Request.Header.Set("Content-Type", "application/json")
			call(c)
			require.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}
}

func TestVideoOperationsRejectInvalidCostBeforeDatabase(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	for _, body := range []string{
		`{"actual_cost":-1,"expected_revision":0,"reference":"bill","note":"bad"}`,
		`{"actual_cost":1,"reference":"bill","note":"missing optimistic revision"}`,
		`{"actual_cost":1,"expected_revision":0,"note":"missing supplier reference"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1})
			c.Request = httptest.NewRequest(http.MethodPost, "/videos/test/reconcile", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.ReconcileVideoTask(c)
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestAdminVideoOperationsFiltersIncludeOperationalScopes(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/videos?exception=missing_usage&reconciliation_status=pending&group_id=4&account_id=125&provider=seedance", nil)
	f := adminVideoOperationsFilterFromQuery(c)
	require.Equal(t, "missing_usage", f.Exception)
	require.Equal(t, "pending", f.ReconciliationStatus)
	require.EqualValues(t, 4, f.GroupID)
	require.EqualValues(t, 125, f.AccountID)
	require.Equal(t, "seedance", f.Provider)
}

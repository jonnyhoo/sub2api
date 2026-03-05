package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMapCountTokensSelectAccountError_ClaudeCodeOnly(t *testing.T) {
	status, errType, message := mapCountTokensSelectAccountError(service.ErrClaudeCodeOnly)

	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "not_found_error", errType)
	require.Equal(t, "count_tokens endpoint is not supported by upstream", message)
}

func TestMapCountTokensSelectAccountError_Default(t *testing.T) {
	status, errType, message := mapCountTokensSelectAccountError(errors.New("select failed"))

	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "api_error", errType)
	require.Equal(t, "Service temporarily unavailable", message)
}

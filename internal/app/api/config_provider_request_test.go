package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestInvalidProviderRequestReturnsBeforeWritingResponse(t *testing.T) {
	for _, body := range []string{`{`, `{"protocol":"unsupported"}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		_, _, err := decodeProviderRequest(echo.New().NewContext(request, recorder))
		var failure *echo.HTTPError
		if !errors.As(err, &failure) || failure.Code != http.StatusBadRequest || recorder.Body.Len() != 0 {
			t.Fatalf("invalid provider request was not returned to the caller: %v %s", err, recorder.Body.String())
		}
	}
}

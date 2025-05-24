package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	// "github.com/zeyugao/synapse/internal/types" // Not strictly needed for these tests
)

// expectedErrorResponse is the standard JSON error for auth failures.
var expectedErrorResponse = map[string]interface{}{
	"error": map[string]interface{}{
		"message": "You must provide a valid API key. Obtain one from http://helmholtz.cloud",
		"type":    "invalid_request_error",
		"param":   nil,
		"code":    "invalid_api_key",
	},
}

// This is the actual signature of the function in server.go
var originalAuthRequestFunc func(apiToken string) (bool, error)

// Mocked version of authenticateRequest for overriding the URL and simulating errors.
// It will be assigned to the global `authenticateRequest` variable during tests.
func mockedAuthenticateRequest(mockHTTPServerURL string) func(apiToken string) (bool, error) {
	return func(apiToken string) (bool, error) {
		if apiToken == "" { // No token provided
			return false, nil
		}

		// Special token to simulate a client.Do network error
		if apiToken == "network-error-token" {
			// Attempt to connect to a non-existent port to guarantee client.Do error
			req, err := http.NewRequest("GET", "http://localhost:12345/api/v4/user", nil)
			if err != nil { // Should not happen for this static URL
				return false, fmt.Errorf("test setup: failed to create network error request: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+apiToken)
			_, err = (&http.Client{}).Do(req) // This will fail
			return false, fmt.Errorf("simulated network error: %w", err)
		}

		// All other tokens go to the mock HTTP server
		req, err := http.NewRequest("GET", mockHTTPServerURL+"/api/v4/user", nil)
		if err != nil {
			return false, fmt.Errorf("test setup: failed to create request to mock auth server: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiToken)
		req.Header.Set("User-Agent", "synapse-server-test")

		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			// This path would be hit if mockHTTPServerURL itself is bad, not for app logic error.
			return false, fmt.Errorf("test setup: failed to perform request to mock auth server: %w", err)
		}
		defer resp.Body.Close()

		return resp.StatusCode == http.StatusOK, nil
	}
}

func TestHandleAPIRequest_Authentication(t *testing.T) {
	// Suppress log output from the server during tests to keep test output clean
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr) // Restore log output after tests

	// Read the API token from the environment variable
	envToken := os.Getenv("OPENAI_API_KEY")

	// This is the mock external authentication server (e.g., codebase.helmholtz.cloud)
	mockExternalAuthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		authHeader := r.Header.Get("Authorization")
		// Check if the token from env var is provided and matches
		if envToken != "" && authHeader == "Bearer "+envToken {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"username": "testuser_env"})
		} else { // Any other token is considered invalid by the mock external server
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_token"})
		}
	}))
	defer mockExternalAuthServer.Close()

	// Store the original authenticateRequest and defer its restoration
	originalAuthRequestFunc = authenticateRequest
	authenticateRequest = mockedAuthenticateRequest(mockExternalAuthServer.URL)
	defer func() {
		authenticateRequest = originalAuthRequestFunc
	}()

	// Create an instance of our Synapse server (the system under test)
	// The wsAuthKey, version, clientBinaryPath, etc., are not critical for these API auth tests.
	synapseServer := NewServer("test-ws-key", "test-ver", "", false)

	tests := []struct {
		name                 string
		requestURL           string
		httpMethod           string
		requestBody          string
		authorizationHeader  string // Full value of the Authorization header
		expectedStatusCode   int
		expectedErrorPayload *map[string]interface{} // If non-nil, compare response body
	}{
		{
			name:                "Valid token from OPENAI_API_KEY",
			requestURL:          "/v1/models",
			httpMethod:          "GET",
			authorizationHeader: "Bearer " + envToken, // Use token from env
			expectedStatusCode:  http.StatusOK,
		},
		{
			name:                 "Invalid token",
			requestURL:           "/v1/models",
			httpMethod:           "GET",
			authorizationHeader:  "Bearer invalid-token",
			expectedStatusCode:   http.StatusUnauthorized,
			expectedErrorPayload: &expectedErrorResponse,
		},
		{
			name:                 "Missing token (no Authorization header)",
			requestURL:           "/v1/models",
			httpMethod:           "GET",
			authorizationHeader:  "", // No header will be set
			expectedStatusCode:   http.StatusUnauthorized,
			expectedErrorPayload: &expectedErrorResponse,
		},
		{
			name:                 "Malformed Authorization header (Basic auth)",
			requestURL:           "/v1/models",
			httpMethod:           "GET",
			authorizationHeader:  "Basic somecredentials",
			expectedStatusCode:   http.StatusUnauthorized,
			expectedErrorPayload: &expectedErrorResponse,
		},
		{
			name:                 "Malformed Authorization header (Bearer prefix only, no token)",
			requestURL:           "/v1/models",
			httpMethod:           "GET",
			authorizationHeader:  "Bearer ",
			expectedStatusCode:   http.StatusUnauthorized,
			expectedErrorPayload: &expectedErrorResponse,
		},
		{
			name:                "External API call fails (network error simulation)",
			requestURL:          "/v1/models",
			httpMethod:          "GET",
			authorizationHeader: "Bearer network-error-token", // Special token for our mock
			expectedStatusCode:  http.StatusUnauthorized,      // Changed to 401 as per updated server.go logic
			expectedErrorPayload: &expectedErrorResponse,      // Now expect the standard error JSON
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Special handling for the valid token test case
			if tt.name == "Valid token from OPENAI_API_KEY" {
				if envToken == "" {
					t.Skip("OPENAI_API_KEY environment variable not set or empty, skipping test for valid token")
				}
				// Ensure the authorizationHeader for this test case is correctly using the envToken
				// This is already set in the struct, but double-check if modification is needed here
				// No, it's correctly using `authorizationHeader: "Bearer " + envToken` from struct init.
			}

			var bodyReader io.Reader
			if tt.requestBody != "" {
				bodyReader = strings.NewReader(tt.requestBody)
			}
			req := httptest.NewRequest(tt.httpMethod, tt.requestURL, bodyReader)
			if tt.authorizationHeader != "" {
				// For the valid token test, if envToken is empty, this test is skipped.
				// So, tt.authorizationHeader will be "Bearer " which is an invalid header for other tests if not skipped.
				// However, the skip logic ensures this path is not problematic for the valid token test.
				// For other tests, tt.authorizationHeader is hardcoded.
				if tt.name == "Valid token from OPENAI_API_KEY" && envToken == "" {
					// This state should not be reached due to the skip above.
					// If it were, "Bearer " would be sent.
				}
				req.Header.Set("Authorization", tt.authorizationHeader)
			}


			rr := httptest.NewRecorder()
			synapseServer.handleAPIRequest(rr, req) // Directly call the handler

			if status := rr.Code; status != tt.expectedStatusCode {
				t.Errorf("handler returned wrong status code: got %v want %v. Body: %s",
					status, tt.expectedStatusCode, rr.Body.String())
			}

			if tt.expectedErrorPayload != nil {
				var actualBody map[string]interface{}
				if err := json.Unmarshal(rr.Body.Bytes(), &actualBody); err != nil {
					// If the body is empty but we expected an error payload, it's an error
					if rr.Body.Len() == 0 {
						t.Fatalf("Expected error body but got empty body. Status: %d", rr.Code)
					}
					t.Fatalf("Could not unmarshal response body: %v. Raw Body: '%s'", err, rr.Body.String())
				}

				expectedErr := (*tt.expectedErrorPayload)["error"].(map[string]interface{})
				actualErr, ok := actualBody["error"].(map[string]interface{})
				if !ok {
					t.Errorf("Response body does not contain 'error' object: got %v", actualBody)
					return
				}

				if actualErr["code"] != expectedErr["code"] {
					t.Errorf("Error payload mismatch for 'code': got %s want %s. Full actual error: %v",
						actualErr["code"], expectedErr["code"], actualErr)
				}
				if actualErr["message"] != expectedErr["message"] {
					t.Errorf("Error payload mismatch for 'message': got %s want %s. Full actual error: %v",
						actualErr["message"], expectedErr["message"], actualErr)
				}
				// param is expected to be null (nil in Go map from JSON)
				// param is expected to be null (nil in Go map from JSON)
				// Check if 'param' exists in actualErr before comparing, as it might be omitted if nil.
				// And compare its nullity against expectedErr["param"]
				actualParam, actualParamExists := actualErr["param"]
				expectedParamIsNull := expectedErr["param"] == nil

				if expectedParamIsNull {
					if actualParamExists && actualParam != nil {
						t.Errorf("Error payload mismatch for 'param': got %v want nil. Full actual error: %v",
							actualParam, actualErr)
					}
				} else { // expectedParam is not null
					if !actualParamExists || actualParam != expectedErr["param"] {
						t.Errorf("Error payload mismatch for 'param': got %v want %v. Full actual error: %v",
							actualParam, expectedErr["param"], actualErr)
					}
				}


				if actualErr["type"] != expectedErr["type"] {
					t.Errorf("Error payload mismatch for 'type': got %s want %s. Full actual error: %v",
						actualErr["type"], expectedErr["type"], actualErr)
				}

			} else if rr.Code == http.StatusOK && tt.name == "Valid token from OPENAI_API_KEY" {
				// For the valid token case, check if the body is a valid JSON (e.g., a list of models)
				// It should be `{"object":"list","data":[]}` if no models/clients are registered.
				var bodyJson map[string]interface{}
				if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&bodyJson); err != nil {
					t.Errorf("For valid token, expected JSON body, but got error: %v. Body: %s", err, rr.Body.String())
				}
				if _, ok := bodyJson["data"]; !ok {
					t.Errorf("For valid token, expected JSON body with 'data' key, got: %s", rr.Body.String())
				}
			}
		})
	}
}

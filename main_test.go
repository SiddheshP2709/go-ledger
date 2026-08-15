package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestFintechLedger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	initDB()
	router := setupRouter()

	// Helper to create test accounts
	createTestAccount := func(t *testing.T, initialBalance decimal.Decimal) string {
		t.Helper()
		userID := fmt.Sprintf("test_user_%s", uuid.New().String())
		var accountID string
		err := db.QueryRow(
			`INSERT INTO accounts (user_id, balance) VALUES ($1, $2) RETURNING id`,
			userID, initialBalance,
		).Scan(&accountID)
		if err != nil {
			t.Fatalf("Failed to create test account: %v", err)
		}
		return accountID
	}

	// Helper to execute authenticated HTTP requests against Gin router
	execRequest := func(method, path string, body interface{}, authHeader string) *httptest.ResponseRecorder {
		var reqBody []byte
		if body != nil {
			reqBody, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, path, bytes.NewBuffer(reqBody))
		req.Header.Set("Content-Type", "application/json")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. High-Concurrency Stress Test (100 goroutines, Bidirectional)
	t.Run("ConcurrentBidirectionalTransfers", func(t *testing.T) {
		accountAID := createTestAccount(t, decimal.NewFromInt(1000))
		accountBID := createTestAccount(t, decimal.NewFromInt(1000))

		const numGoroutines = 100
		transferAmount := decimal.NewFromInt(10)

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(index int) {
				defer wg.Done()

				var sourceID, destID string
				if index%2 == 0 {
					sourceID, destID = accountAID, accountBID
				} else {
					sourceID, destID = accountBID, accountAID
				}

				payload := TransferRequest{
					IdempotencyKey:       uuid.New().String(),
					SourceAccountID:      sourceID,
					DestinationAccountID: destID,
					Amount:               transferAmount,
				}

				w := execRequest("POST", "/transfers", payload, "Bearer test-token")
				if w.Code != http.StatusOK {
					t.Errorf("Goroutine %d transfer failed with status %d: %s", index, w.Code, w.Body.String())
				}
			}(i)
		}

		wg.Wait()

		var finalBalanceA, finalBalanceB decimal.Decimal
		_ = db.QueryRow(`SELECT balance FROM accounts WHERE id = $1`, accountAID).Scan(&finalBalanceA)
		_ = db.QueryRow(`SELECT balance FROM accounts WHERE id = $1`, accountBID).Scan(&finalBalanceB)

		expectedBalance := decimal.NewFromInt(1000)
		if !finalBalanceA.Equal(expectedBalance) {
			t.Errorf("Account A final balance mismatch: expected %s, got %s", expectedBalance.String(), finalBalanceA.String())
		}
		if !finalBalanceB.Equal(expectedBalance) {
			t.Errorf("Account B final balance mismatch: expected %s, got %s", expectedBalance.String(), finalBalanceB.String())
		}
	})

	// 2. Insufficient Funds Test
	t.Run("InsufficientFunds", func(t *testing.T) {
		accountAID := createTestAccount(t, decimal.NewFromInt(50))
		accountBID := createTestAccount(t, decimal.NewFromInt(100))

		payload := TransferRequest{
			IdempotencyKey:       uuid.New().String(),
			SourceAccountID:      accountAID,
			DestinationAccountID: accountBID,
			Amount:               decimal.NewFromInt(500), // Exceeds balance
		}

		w := execRequest("POST", "/transfers", payload, "Bearer test-token")
		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 Bad Request for insufficient funds, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 3. Duplicate Idempotency Key Test
	t.Run("DuplicateIdempotencyKey", func(t *testing.T) {
		accountAID := createTestAccount(t, decimal.NewFromInt(500))
		accountBID := createTestAccount(t, decimal.NewFromInt(100))
		sharedKey := uuid.New().String()

		payload := TransferRequest{
			IdempotencyKey:       sharedKey,
			SourceAccountID:      accountAID,
			DestinationAccountID: accountBID,
			Amount:               decimal.NewFromInt(50),
		}

		// First request should succeed
		w1 := execRequest("POST", "/transfers", payload, "Bearer test-token")
		if w1.Code != http.StatusOK {
			t.Fatalf("First transfer failed with status %d: %s", w1.Code, w1.Body.String())
		}

		// Second request with same idempotency key must fail with 409 Conflict
		w2 := execRequest("POST", "/transfers", payload, "Bearer test-token")
		if w2.Code != http.StatusConflict {
			t.Errorf("Expected 409 Conflict on duplicate idempotency key, got %d: %s", w2.Code, w2.Body.String())
		}
	})

	// 4. Negative & Zero Amount Rejection Test
	t.Run("NegativeAndZeroAmounts", func(t *testing.T) {
		accountAID := createTestAccount(t, decimal.NewFromInt(500))
		accountBID := createTestAccount(t, decimal.NewFromInt(100))

		negativePayload := TransferRequest{
			IdempotencyKey:       uuid.New().String(),
			SourceAccountID:      accountAID,
			DestinationAccountID: accountBID,
			Amount:               decimal.NewFromInt(-50),
		}
		wNeg := execRequest("POST", "/transfers", negativePayload, "Bearer test-token")
		if wNeg.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 Bad Request for negative amount, got %d", wNeg.Code)
		}

		zeroPayload := TransferRequest{
			IdempotencyKey:       uuid.New().String(),
			SourceAccountID:      accountAID,
			DestinationAccountID: accountBID,
			Amount:               decimal.Zero,
		}
		wZero := execRequest("POST", "/transfers", zeroPayload, "Bearer test-token")
		if wZero.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 Bad Request for zero amount, got %d", wZero.Code)
		}
	})

	// 5. Self-Transfer Prevention Test
	t.Run("SelfTransferPrevention", func(t *testing.T) {
		accountAID := createTestAccount(t, decimal.NewFromInt(500))

		payload := TransferRequest{
			IdempotencyKey:       uuid.New().String(),
			SourceAccountID:      accountAID,
			DestinationAccountID: accountAID, // Self transfer
			Amount:               decimal.NewFromInt(50),
		}

		w := execRequest("POST", "/transfers", payload, "Bearer test-token")
		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 Bad Request for self-transfer, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 6. Authentication Middleware Enforcement Test
	t.Run("AuthMiddlewareEnforcement", func(t *testing.T) {
		accountAID := createTestAccount(t, decimal.NewFromInt(500))

		// Missing Authorization header -> 401
		wMissing := execRequest("GET", fmt.Sprintf("/accounts/%s", accountAID), nil, "")
		if wMissing.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 Unauthorized for missing auth header, got %d", wMissing.Code)
		}

		// Malformed header -> 401
		wMalformed := execRequest("GET", fmt.Sprintf("/accounts/%s", accountAID), nil, "Basic dXNlcjpwYXNz")
		if wMalformed.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 Unauthorized for malformed auth header, got %d", wMalformed.Code)
		}

		// Valid header -> 200
		wValid := execRequest("GET", fmt.Sprintf("/accounts/%s", accountAID), nil, "Bearer valid-token")
		if wValid.Code != http.StatusOK {
			t.Errorf("Expected 200 OK for valid auth header, got %d", wValid.Code)
		}
	})

	// 7. Duplicate User ID Registration Conflict Test
	t.Run("DuplicateUserIDConflict", func(t *testing.T) {
		uniqueUserID := fmt.Sprintf("clash_user_%s", uuid.New().String())

		// First registration should succeed (201)
		payload := CreateAccountRequest{UserID: uniqueUserID}
		w1 := execRequest("POST", "/accounts", payload, "")
		if w1.Code != http.StatusCreated {
			t.Fatalf("First account registration failed with status %d: %s", w1.Code, w1.Body.String())
		}

		// Second registration with same user_id must fail with 409 Conflict
		w2 := execRequest("POST", "/accounts", payload, "")
		if w2.Code != http.StatusConflict {
			t.Errorf("Expected 409 Conflict for duplicate user_id, got %d: %s", w2.Code, w2.Body.String())
		}
	})
}

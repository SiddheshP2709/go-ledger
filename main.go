package main

import (
	// database/sql provides a generic interface around SQL (or SQL-like) databases.
	"database/sql"
	// fmt implements formatted I/O like printing to the console.
	"fmt"
	// log provides simple logging functions.
	"log"
	// net/http provides HTTP client and server implementations and HTTP status codes.
	"net/http"
	// strings provides string manipulation functions.
	"strings"
	// time provides functionality for measuring and displaying time.
	"time"

	// gin is a high-performance HTTP web framework written in Go.
	"github.com/gin-gonic/gin"
	// golang-migrate handles versioned database migrations.
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	// lib/pq is the Postgres driver for database/sql and provides pq.Error type assertion.
	"github.com/lib/pq"
	// decimal provides arbitrary-precision fixed-point decimal numbers to prevent floating-point rounding errors.
	"github.com/shopspring/decimal"
)

// Global database connection pool pointer
var db *sql.DB

// CreateAccountRequest represents the JSON payload expected for POST /accounts
type CreateAccountRequest struct {
	UserID string `json:"user_id" binding:"required"`
}

// CreateAccountResponse represents the JSON response returned for POST /accounts
type CreateAccountResponse struct {
	Message   string `json:"message"`
	AccountID string `json:"account_id"`
}

// DepositRequest represents the JSON payload expected for POST /accounts/deposit
type DepositRequest struct {
	AccountID string          `json:"account_id" binding:"required"`
	Amount    decimal.Decimal `json:"amount" binding:"required"`
}

// DepositResponse represents the JSON response returned for POST /accounts/deposit
type DepositResponse struct {
	Message    string          `json:"message"`
	AccountID  string          `json:"account_id"`
	NewBalance decimal.Decimal `json:"new_balance"`
}

// TransferRequest represents the JSON payload expected for POST /transfers
type TransferRequest struct {
	IdempotencyKey       string          `json:"idempotency_key" binding:"required,uuid"`
	SourceAccountID      string          `json:"source_account_id" binding:"required"`
	DestinationAccountID string          `json:"destination_account_id" binding:"required"`
	Amount               decimal.Decimal `json:"amount" binding:"required"`
}

// TransferResponse represents the JSON response returned for POST /transfers
type TransferResponse struct {
	Message              string          `json:"message"`
	IdempotencyKey       string          `json:"idempotency_key"`
	SourceAccountID      string          `json:"source_account_id"`
	DestinationAccountID string          `json:"destination_account_id"`
	Amount               decimal.Decimal `json:"amount"`
	SourceNewBalance     decimal.Decimal `json:"source_new_balance"`
	DestNewBalance       decimal.Decimal `json:"dest_new_balance"`
}

// AccountResponse represents the JSON payload returned when querying an account
type AccountResponse struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	Balance   decimal.Decimal `json:"balance"`
	CreatedAt time.Time       `json:"created_at"`
}

// LedgerEntryResponse represents a historical ledger transaction record for an account
type LedgerEntryResponse struct {
	ID                   string          `json:"id"`
	SourceAccountID      *string         `json:"source_account_id,omitempty"`
	DestinationAccountID *string         `json:"destination_account_id,omitempty"`
	Amount               decimal.Decimal `json:"amount"`
	EntryType            string          `json:"entry_type"`
	CreatedAt            time.Time       `json:"created_at"`
}

// initDB establishes the database connection and runs database migrations using golang-migrate
func initDB() {
	connStr := "postgres://root:secretpassword@localhost:5433/wallet_ledger?sslmode=disable"

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatalf("Error opening database connection: %v\n", err)
	}

	if err = db.Ping(); err != nil {
		log.Fatalf("Error pinging database: %v\n", err)
	}

	// Configure connection pool settings for resilient concurrent workloads
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	fmt.Println("Successfully connected to PostgreSQL database!")

	// Programmatically initialize the postgres migration driver from existing db handle
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		log.Fatalf("Could not create postgres migration driver: %v\n", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		"file://db/migrations",
		"postgres", driver,
	)
	if err != nil {
		log.Fatalf("Could not initialize migrate instance: %v\n", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatalf("Could not run database migrations: %v\n", err)
	}

	fmt.Println("Database migrations applied successfully.")
}

// createAccountHandler handles POST /accounts
func createAccountHandler(c *gin.Context) {
	var req CreateAccountRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body. 'user_id' is required."})
		return
	}

	var newAccountID string
	query := `INSERT INTO accounts (user_id) VALUES ($1) RETURNING id`
	err := db.QueryRowContext(c.Request.Context(), query, req.UserID).Scan(&newAccountID)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "An account with this user_id already exists."})
			return
		}
		log.Printf("ERROR [createAccountHandler]: failed to insert account for user %s: %v\n", req.UserID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account. Please try again later."})
		return
	}

	c.JSON(http.StatusCreated, CreateAccountResponse{
		Message:   "Account created successfully",
		AccountID: newAccountID,
	})
}

// depositHandler handles POST /accounts/deposit
func depositHandler(c *gin.Context) {
	var req DepositRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body. 'account_id' and 'amount' are required."})
		return
	}

	if req.Amount.LessThanOrEqual(decimal.Zero) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Deposit amount must be greater than 0."})
		return
	}

	// Begin an ACID transaction with explicit READ COMMITTED isolation
	// Explicitly using READ COMMITTED with FOR UPDATE (Pessimistic Locking) to avoid serialization anomaly retries.
	tx, err := db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		log.Printf("ERROR [depositHandler]: failed to begin transaction: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process deposit."})
		return
	}
	defer tx.Rollback()

	// Lock the account row using SELECT ... FOR UPDATE
	var currentBalance decimal.Decimal
	lockQuery := `SELECT balance FROM accounts WHERE id = $1 FOR UPDATE`
	err = tx.QueryRowContext(c.Request.Context(), lockQuery, req.AccountID).Scan(&currentBalance)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "Account not found."})
			return
		}
		log.Printf("ERROR [depositHandler]: failed to lock account %s: %v\n", req.AccountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process deposit."})
		return
	}

	// Update balance and retrieve the exact stored balance from PostgreSQL
	var newBalance decimal.Decimal
	updateQuery := `UPDATE accounts SET balance = balance + $1 WHERE id = $2 RETURNING balance`
	err = tx.QueryRowContext(c.Request.Context(), updateQuery, req.Amount, req.AccountID).Scan(&newBalance)
	if err != nil {
		log.Printf("ERROR [depositHandler]: failed to update balance for account %s: %v\n", req.AccountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update account balance."})
		return
	}

	// Record ledger audit entry
	insertLedgerQuery := `
		INSERT INTO ledger_entries (destination_account_id, amount, entry_type) 
		VALUES ($1, $2, $3)
	`
	_, err = tx.ExecContext(c.Request.Context(), insertLedgerQuery, req.AccountID, req.Amount, "DEPOSIT")
	if err != nil {
		log.Printf("ERROR [depositHandler]: failed to record ledger entry for account %s: %v\n", req.AccountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record transaction."})
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("ERROR [depositHandler]: failed to commit transaction for account %s: %v\n", req.AccountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit deposit."})
		return
	}

	c.JSON(http.StatusOK, DepositResponse{
		Message:    "Deposit successful",
		AccountID:  req.AccountID,
		NewBalance: newBalance,
	})
}

// transferHandler handles POST /transfers
func transferHandler(c *gin.Context) {
	var req TransferRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body. 'idempotency_key' (UUID), 'source_account_id', 'destination_account_id', and 'amount' are required."})
		return
	}

	if req.Amount.LessThanOrEqual(decimal.Zero) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Transfer amount must be greater than 0."})
		return
	}

	// Prevent self-transfers
	if req.SourceAccountID == req.DestinationAccountID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Self-transfers are not allowed. Source and destination accounts must be different."})
		return
	}

	// Begin an ACID transaction
	// Explicitly using READ COMMITTED with FOR UPDATE (Pessimistic Locking) to avoid serialization anomaly retries.
	tx, err := db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		log.Printf("ERROR [transferHandler]: failed to begin transaction: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start transfer transaction."})
		return
	}
	defer tx.Rollback()

	// Stripe-level Idempotency Check
	insertKeyQuery := `INSERT INTO idempotency_keys (key) VALUES ($1)`
	_, err = tx.ExecContext(c.Request.Context(), insertKeyQuery, req.IdempotencyKey)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "Idempotent request: Transfer already processed."})
			return
		}
		log.Printf("ERROR [transferHandler]: failed to insert idempotency key %s: %v\n", req.IdempotencyKey, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record idempotency key."})
		return
	}

	// Deadlock Prevention: Deterministic total lock ordering
	firstID, secondID := req.SourceAccountID, req.DestinationAccountID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	var firstBalance, secondBalance decimal.Decimal
	lockQuery := `SELECT balance FROM accounts WHERE id = $1 FOR UPDATE`

	err = tx.QueryRowContext(c.Request.Context(), lockQuery, firstID).Scan(&firstBalance)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Account not found: %s", firstID)})
			return
		}
		log.Printf("ERROR [transferHandler]: failed to lock account %s: %v\n", firstID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to lock account for transfer."})
		return
	}

	err = tx.QueryRowContext(c.Request.Context(), lockQuery, secondID).Scan(&secondBalance)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Account not found: %s", secondID)})
			return
		}
		log.Printf("ERROR [transferHandler]: failed to lock account %s: %v\n", secondID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to lock account for transfer."})
		return
	}

	// Map balances back to source and destination
	var sourceBalance decimal.Decimal
	if req.SourceAccountID == firstID {
		sourceBalance = firstBalance
	} else {
		sourceBalance = secondBalance
	}

	// Verify source has sufficient funds
	if sourceBalance.LessThan(req.Amount) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Insufficient funds. Source account balance is %s, transfer amount is %s.", sourceBalance.String(), req.Amount.String()),
		})
		return
	}

	// Update source and destination balances and retrieve the exact stored values from PostgreSQL
	var newSourceBalance, newDestBalance decimal.Decimal

	updateSourceQuery := `UPDATE accounts SET balance = balance - $1 WHERE id = $2 RETURNING balance`
	err = tx.QueryRowContext(c.Request.Context(), updateSourceQuery, req.Amount, req.SourceAccountID).Scan(&newSourceBalance)
	if err != nil {
		log.Printf("ERROR [transferHandler]: failed to update source account %s: %v\n", req.SourceAccountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update source account balance."})
		return
	}

	updateDestQuery := `UPDATE accounts SET balance = balance + $1 WHERE id = $2 RETURNING balance`
	err = tx.QueryRowContext(c.Request.Context(), updateDestQuery, req.Amount, req.DestinationAccountID).Scan(&newDestBalance)
	if err != nil {
		log.Printf("ERROR [transferHandler]: failed to update destination account %s: %v\n", req.DestinationAccountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update destination account balance."})
		return
	}

	// Insert audit record into ledger_entries
	insertLedgerQuery := `
		INSERT INTO ledger_entries (source_account_id, destination_account_id, amount, entry_type) 
		VALUES ($1, $2, $3, $4)
	`
	_, err = tx.ExecContext(c.Request.Context(), insertLedgerQuery, req.SourceAccountID, req.DestinationAccountID, req.Amount, "TRANSFER")
	if err != nil {
		log.Printf("ERROR [transferHandler]: failed to record ledger entry: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record ledger entry."})
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("ERROR [transferHandler]: failed to commit transfer: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit transfer."})
		return
	}

	c.JSON(http.StatusOK, TransferResponse{
		Message:              "Transfer successful",
		IdempotencyKey:       req.IdempotencyKey,
		SourceAccountID:      req.SourceAccountID,
		DestinationAccountID: req.DestinationAccountID,
		Amount:               req.Amount,
		SourceNewBalance:     newSourceBalance,
		DestNewBalance:       newDestBalance,
	})
}

// getAccountHandler handles GET /accounts/:id
func getAccountHandler(c *gin.Context) {
	accountID := c.Param("id")

	var account AccountResponse
	query := `SELECT id, user_id, balance, created_at FROM accounts WHERE id = $1`
	err := db.QueryRowContext(c.Request.Context(), query, accountID).Scan(
		&account.ID,
		&account.UserID,
		&account.Balance,
		&account.CreatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "Account not found."})
			return
		}
		log.Printf("ERROR [getAccountHandler]: failed to fetch account %s: %v\n", accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve account details."})
		return
	}

	c.JSON(http.StatusOK, account)
}

// getAccountTransactionsHandler handles GET /accounts/:id/transactions
func getAccountTransactionsHandler(c *gin.Context) {
	accountID := c.Param("id")

	query := `
		SELECT id, source_account_id, destination_account_id, amount, entry_type, created_at 
		FROM ledger_entries 
		WHERE source_account_id = $1 OR destination_account_id = $1 
		ORDER BY created_at DESC
	`
	rows, err := db.QueryContext(c.Request.Context(), query, accountID)
	if err != nil {
		log.Printf("ERROR [getAccountTransactionsHandler]: failed to query ledger for account %s: %v\n", accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve transaction history."})
		return
	}
	defer rows.Close()

	entries := make([]LedgerEntryResponse, 0)

	for rows.Next() {
		var entry LedgerEntryResponse
		err := rows.Scan(
			&entry.ID,
			&entry.SourceAccountID,
			&entry.DestinationAccountID,
			&entry.Amount,
			&entry.EntryType,
			&entry.CreatedAt,
		)
		if err != nil {
			log.Printf("ERROR [getAccountTransactionsHandler]: failed to scan row: %v\n", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse transaction history."})
			return
		}
		entries = append(entries, entry)
	}

	if err = rows.Err(); err != nil {
		log.Printf("ERROR [getAccountTransactionsHandler]: row iteration error: %v\n", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error processing transaction history."})
		return
	}

	c.JSON(http.StatusOK, entries)
}

// authMiddleware simulates token-based authentication.
// In a production environment, this would decode and cryptographically verify a JWT from an Identity Provider (e.g., Auth0, Keycloak, Cognito).
func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Missing or malformed Authorization header. Expected 'Bearer <token>'"})
			return
		}

		token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid bearer token"})
			return
		}

		c.Next()
	}
}

// setupRouter configures and returns the Gin engine with public and protected API routes
func setupRouter() *gin.Engine {
	router := gin.Default()

	// Public endpoint: POST /accounts (simulates open user onboarding / registration)
	router.POST("/accounts", createAccountHandler)

	// Protected endpoints: Require Authorization header
	protected := router.Group("/")
	protected.Use(authMiddleware())
	{
		// Route 2: POST /accounts/deposit - Deposit funds into an existing account
		protected.POST("/accounts/deposit", depositHandler)

		// Route 3: POST /transfers - Transfer funds between two accounts
		protected.POST("/transfers", transferHandler)

		// Route 4: GET /accounts/:id - Fetch account details by ID
		protected.GET("/accounts/:id", getAccountHandler)

		// Route 5: GET /accounts/:id/transactions - Fetch ledger transaction history for an account
		protected.GET("/accounts/:id/transactions", getAccountTransactionsHandler)
	}

	return router
}

func main() {
	// Initialize database connection and schema
	initDB()
	defer db.Close()

	// Initialize Gin router
	router := setupRouter()

	port := ":8080"
	fmt.Printf("Server listening on http://localhost%s\n", port)
	if err := router.Run(port); err != nil {
		log.Fatalf("Failed to run server: %v\n", err)
	}
}

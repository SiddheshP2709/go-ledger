# 💳 FinTech Ledger API

[![Go Version](https://img.shields.io/badge/Go-1.20+-00ADD8?style=flat&logo=go)](https://golang.org/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-15-336791?style=flat&logo=postgresql)](https://www.postgresql.org/)
[![Gin Framework](https://img.shields.io/badge/Gin-v1.12-008ECF?style=flat&logo=gin)](https://gin-gonic.com/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A financial ledger API built with **Go (Golang)**, **Gin Framework**, and **PostgreSQL** to handle account creation, deposits, and transfers with concurrency controls, exact decimal math, idempotency, and audit logging.

---

## 🏛️ Architecture & Core Design

```
                  ┌─────────────────────────────────────────────────────────┐
                  │               Upstream API Gateway / IDP                │
                  │             (Token Validation Upstream)                 │
                  └────────────────────────────┬────────────────────────────┘
                                               │ HTTP + Bearer Token
                                               ▼
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                 FinTech Ledger Service (Go)                                 │
│                                                                                             │
│  ┌───────────────────────┐   ┌───────────────────────────┐   ┌───────────────────────────┐  │
│  │    authMiddleware     │──▶│   Deterministic Sorting   │──▶│     Fixed-Point Math      │  │
│  │ (Bearer Token Check)  │   │(Deadlock Prevention Logic)│   │  (shopspring/decimal)     │  │
│  └───────────────────────┘   └───────────────────────────┘   └───────────────────────────┘  │
└──────────────────────────────────────────────┬──────────────────────────────────────────────┘
                                               │ Single Transaction (db.BeginTx)
                                               ▼
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                  PostgreSQL 15 Database                                     │
│                                                                                             │
│  ┌───────────────────────────┐  ┌───────────────────────────┐  ┌─────────────────────────┐  │
│  │    idempotency_keys       │  │         accounts          │  │     ledger_entries      │  │
│  │ (Unique 23505 Handling)   │  │(Pessimistic FOR UPDATE)   │  │ (Immutable Audit Log)   │  │
│  │                           │  │(CHECK balance >= 0)       │  │                         │  │
│  └───────────────────────────┘  └───────────────────────────┘  └─────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

---

### 1. Concurrency Control & Deadlock Prevention

#### 🔹 `READ COMMITTED` + Pessimistic Row Locking (`SELECT ... FOR UPDATE`)
All balance updates are executed within a database transaction using:
```go
// Explicitly using READ COMMITTED with FOR UPDATE (Pessimistic Locking) to avoid serialization anomaly retries.
tx, err := db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
```
* **Why not `SERIALIZABLE`?** Under high concurrency, `SERIALIZABLE` isolation in PostgreSQL can abort concurrent transactions modifying the same accounts with serialization failure errors (`SQLSTATE 40001`), requiring retry logic in the application.
* **Approach:** Using `READ COMMITTED` with `SELECT ... FOR UPDATE` queues concurrent balance updates at the database row lock level, avoiding serialization aborts.

#### 🔹 Preventing Two-Account Circular Deadlocks via Lexicographical Sorting
In two-party transfers, a circular wait deadlock can occur if:
* **Thread A** transfers from **Account 1** $\rightarrow$ **Account 2** (Locks Account 1, waits for Account 2)
* **Thread B** transfers from **Account 2** $\rightarrow$ **Account 1** (Locks Account 2, waits for Account 1)

* **Approach:** The transfer handler sorts both account UUID strings in alphabetical order prior to acquiring locks:
```go
firstID, secondID := req.SourceAccountID, req.DestinationAccountID
if firstID > secondID {
    firstID, secondID = secondID, firstID
}
// Always acquire lock on firstID first, then secondID
```
Enforcing a consistent locking order across all requests prevents the classic circular wait condition for two-account transfers.

---

### 2. Automated Testing (`main_test.go`)

The test suite in `main_test.go` verifies functionality across positive, negative, and concurrent scenarios:
* **Concurrent Bidirectional Transfers:** Spins up 100 concurrent goroutines (50 transfers $A \rightarrow B$ of $10, and 50 transfers $B \rightarrow A$ of $10$) against two accounts initialized with $1,000.00 each.
* **Verification:** Confirms that after all 100 concurrent requests finish, both accounts retain their exact starting balance of **$1,000.0000** without race conditions or deadlocks.
* **Additional Test Cases:**
  - Insufficient balance rejection (`400 Bad Request`).
  - Duplicate idempotency key detection (`409 Conflict`).
  - Negative and zero amount validation (`400 Bad Request`).
  - Self-transfer prevention (`400 Bad Request`).
  - Missing or malformed authorization headers (`401 Unauthorized`).
  - Duplicate `user_id` registration conflict (`409 Conflict`).

```bash
go test -v
```

---

### 3. Precision Arithmetic

Binary floating-point numbers (`float64`) can introduce rounding inaccuracies in financial calculations (e.g., `0.1 + 0.2 != 0.3`).

* **Implementation:** Balances, transfer amounts, and ledger records use [`github.com/shopspring/decimal`](https://github.com/shopspring/decimal), storing values as PostgreSQL `DECIMAL(18, 4)`.
* **Database Synchronization:** Balance updates use SQL `RETURNING balance` to scan the exact stored value from PostgreSQL directly into the response payload.

---

### 4. Idempotency Handling

To avoid double-processing if a client retries a request after a network timeout:
* `POST /transfers` accepts a client-provided UUID `idempotency_key`.
* The key is inserted into the `idempotency_keys` table inside the transaction:
  ```sql
  INSERT INTO idempotency_keys (key) VALUES ($1);
  ```
* If a request with the same key is submitted again, PostgreSQL returns a unique constraint violation (`SQLSTATE 23505`).
* The handler catches this error, rolls back the transaction, and returns **HTTP 409 Conflict** (`"Idempotent request: Transfer already processed."`).

---

### 5. Validation & Database Constraints

* **API Layer Validation:** Validates that amount is strictly greater than zero (`req.Amount.LessThanOrEqual(decimal.Zero)`) before querying the database (HTTP 400).
* **Transaction Layer Check:** Confirms the source account balance is sufficient (`sourceBalance >= req.Amount`) under the row lock.
* **Database Constraint:** The `accounts` table includes a check constraint:
  ```sql
  CHECK (balance >= 0)
  ```
  This serves as an additional safeguard at the database level against negative balances.

---

### 6. Schema Migrations

Database schema initialization is version-controlled using [`golang-migrate/migrate`](https://github.com/golang-migrate/migrate):
* Migration files located in `db/migrations/`:
  - `000001_init_schema.up.sql`: Sets up `pgcrypto`, `accounts`, `ledger_entries`, and `idempotency_keys`.
  - `000001_init_schema.down.sql`: Drops tables in reverse order.
* Migrations run on startup inside `initDB()`, handling `migrate.ErrNoChange` when already up to date.

---

### 7. Authentication Middleware

* An `authMiddleware` verifies that incoming requests on protected routes include a valid `Authorization: Bearer <token>` header.
* In a deployed system, this service would sit behind an API Gateway or Identity Provider that validates JWTs and passes authenticated requests downstream.
* `POST /accounts` remains open for user onboarding, while balance and transfer routes require authentication.

---

## 🗄️ Database Schema

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 1. Accounts Table
CREATE TABLE accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(255) NOT NULL UNIQUE,
    balance DECIMAL(18, 4) NOT NULL DEFAULT 0.0000 CHECK (balance >= 0),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 2. Immutable Ledger Entries (Double-Entry Bookkeeping)
CREATE TABLE ledger_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_account_id UUID REFERENCES accounts(id),
    destination_account_id UUID REFERENCES accounts(id),
    amount DECIMAL(18, 4) NOT NULL,
    entry_type VARCHAR(50) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 3. Idempotency Tracking Table
CREATE TABLE idempotency_keys (
    key VARCHAR(255) PRIMARY KEY,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

---

## 📡 API Endpoints Documentation

Protected endpoints require the HTTP header:
```http
Authorization: Bearer <token>
```

| Method | Path | Access | Description |
|---|---|---|---|
| `POST` | `/accounts` | **Public** | Creates a new user account with an initial zero balance. |
| `GET` | `/accounts/:id` | **Protected** | Retrieves account balance, owner ID, and creation timestamp (Requires `Authorization` header). |
| `POST` | `/accounts/deposit` | **Protected** | Deposits funds into an account and records a ledger entry (Requires `Authorization` header). |
| `POST` | `/transfers` | **Protected** | Transfers funds between two accounts with lock ordering & idempotency handling (Requires `Authorization` header). |
| `GET` | `/accounts/:id/transactions` | **Protected** | Retrieves the ledger transaction history for an account (Requires `Authorization` header). |

---

### Request & Response Examples

#### 1. Create Account (`POST /accounts`)
* **Request:**
  ```bash
  curl -X POST http://localhost:8080/accounts \
    -H "Content-Type: application/json" \
    -d '{"user_id": "alice_99"}'
  ```
* **Response (`201 Created`):**
  ```json
  {
    "message": "Account created successfully",
    "account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a"
  }
  ```

#### 2. Get Account (`GET /accounts/:id`)
* **Request:**
  ```bash
  curl -X GET http://localhost:8080/accounts/530315c0-9eb0-4b6e-8db1-d2f8d860df0a \
    -H "Authorization: Bearer test-token"
  ```
* **Response (`200 OK`):**
  ```json
  {
    "id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
    "user_id": "alice_99",
    "balance": "1000.0000",
    "created_at": "2026-08-15T17:02:40.123456Z"
  }
  ```

#### 3. Deposit Funds (`POST /accounts/deposit`)
* **Request:**
  ```bash
  curl -X POST http://localhost:8080/accounts/deposit \
    -H "Authorization: Bearer test-token" \
    -H "Content-Type: application/json" \
    -d '{
      "account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
      "amount": "500.0000"
    }'
  ```
* **Response (`200 OK`):**
  ```json
  {
    "message": "Deposit successful",
    "account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
    "new_balance": "1500.0000"
  }
  ```

#### 4. Transfer Funds (`POST /transfers`)
* **Request:**
  ```bash
  curl -X POST http://localhost:8080/transfers \
    -H "Authorization: Bearer test-token" \
    -H "Content-Type: application/json" \
    -d '{
      "idempotency_key": "d3b07384-d113-494a-b8e7-66a88b50f757",
      "source_account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
      "destination_account_id": "cf5bce00-d8d2-4c0d-a177-29ea8c48466f",
      "amount": "250.0000"
    }'
  ```
* **Response (`200 OK`):**
  ```json
  {
    "message": "Transfer successful",
    "idempotency_key": "d3b07384-d113-494a-b8e7-66a88b50f757",
    "source_account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
    "destination_account_id": "cf5bce00-d8d2-4c0d-a177-29ea8c48466f",
    "amount": "250.0000",
    "source_new_balance": "1250.0000",
    "dest_new_balance": "250.0000"
  }
  ```
* **Duplicate Request Response (`409 Conflict`):**
  ```json
  {
    "error": "Idempotent request: Transfer already processed."
  }
  ```

#### 5. Get Ledger History (`GET /accounts/:id/transactions`)
* **Request:**
  ```bash
  curl -X GET http://localhost:8080/accounts/530315c0-9eb0-4b6e-8db1-d2f8d860df0a/transactions \
    -H "Authorization: Bearer test-token"
  ```
* **Response (`200 OK`):**
  ```json
  [
    {
      "id": "e6c9d52d-17eb-4c8d-97b9-cc3c7da710a4",
      "source_account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
      "destination_account_id": "cf5bce00-d8d2-4c0d-a177-29ea8c48466f",
      "amount": "250.0000",
      "entry_type": "TRANSFER",
      "created_at": "2026-08-15T17:21:56.123456Z"
    },
    {
      "id": "9ee5c0b5-2972-41e5-a74f-bd7688b2ef40",
      "source_account_id": null,
      "destination_account_id": "530315c0-9eb0-4b6e-8db1-d2f8d860df0a",
      "amount": "1000.0000",
      "entry_type": "DEPOSIT",
      "created_at": "2026-08-15T11:40:48.730548Z"
    }
  ]
  ```

---

## ⚡ Getting Started

### Prerequisites
- [Docker Desktop](https://www.docker.com/products/docker-desktop/) installed & running
- [Go 1.20+](https://golang.org/dl/) installed

### 1. Start PostgreSQL via Docker
```bash
docker compose up -d
```

### 2. Run the Test Suite
```bash
go test -v
```

### 3. Run the Server
```bash
go run main.go
```
The server connects to PostgreSQL, applies migrations from `db/migrations/`, and listens on `http://localhost:8080`.

---

## 🔭 Known Limitations & Next Steps

- **Idempotency replay:** Duplicate requests currently return `409 Conflict` rather than replaying the original successful response. A production version would cache and return the original result on retry.
- **No idempotency-key-to-payload binding:** A retried key with a *different* payload amount isn't currently detected as a parameter mutation.
- **Missing database indexes:** `ledger_entries.source_account_id` and `destination_account_id` do not have explicit indexes yet; history queries will perform sequential scans as the table grows.
- **Auth is a placeholder:** `authMiddleware` checks bearer header format only, not a cryptographically signed token (see §7).
- **Observability & rate limiting:** No rate limiting, structured metrics (Prometheus), or distributed tracing (OpenTelemetry) configured yet.

---

## 📜 License
This project is licensed under the MIT License.

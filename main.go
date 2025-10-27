package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onflow/cadence"
	"github.com/onflow/flow-go-sdk"
	"github.com/onflow/flow-go-sdk/access/grpc"
	"github.com/onflow/flow-go-sdk/crypto"
	"github.com/shopspring/decimal"
)

const (
	flowAccessNode  = "access.devnet.nodes.onflow.org:9000"
	contractAddress = "0xe3f7e4d39675d8d3"

	updateInterval = 5 * time.Minute

	coinGeckoAPI = "https://api.coingecko.com/api/v3/simple/price?ids=flow&vs_currencies=usd"
)

type PriceResponse struct {
	Flow struct {
		USD float64 `json:"usd"`
	} `json:"flow"`
}

type OracleUpdater struct {
	flowClient *grpc.Client
	account    *flow.Account
	privateKey crypto.PrivateKey
	signer     crypto.Signer
	db         *pgxpool.Pool
}

func NewOracleUpdater(privateKeyHex string, accountAddress string, databaseURL string) (*OracleUpdater, error) {

	flowClient, err := grpc.NewClient(flowAccessNode)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Flow: %w", err)
	}

	privateKey, err := crypto.DecodePrivateKeyHex(crypto.ECDSA_P256, privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode private key: %w", err)
	}

	signer, err := crypto.NewInMemorySigner(privateKey, crypto.SHA2_256)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	address := flow.HexToAddress(accountAddress)
	account, err := flowClient.GetAccount(context.Background(), address)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	dbConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	dbConfig.MaxConns = 10
	dbConfig.MinConns = 2
	dbConfig.MaxConnLifetime = time.Hour
	dbConfig.MaxConnIdleTime = 30 * time.Minute

	db, err := pgxpool.NewWithConfig(context.Background(), dbConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := db.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("✅ Database connection established")

	return &OracleUpdater{
		flowClient: flowClient,
		account:    account,
		privateKey: privateKey,
		signer:     signer,
		db:         db,
	}, nil
}

func (o *OracleUpdater) GetFlowPrice() (float64, error) {
	resp, err := http.Get(coinGeckoAPI)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch price: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response: %w", err)
	}

	var priceResp PriceResponse
	if err := json.Unmarshal(body, &priceResp); err != nil {
		return 0, fmt.Errorf("failed to parse price: %w", err)
	}

	return priceResp.Flow.USD, nil
}

var lastTxID string

func (o *OracleUpdater) UpdatePriceOnChain(price float64) error {
	ctx := context.Background()

	script := fmt.Sprintf(`
import PriceOracle from %s

transaction(newPrice: UFix64) {
    prepare(signer: auth(Storage) &Account) {
        let admin = signer.storage.borrow<&PriceOracle.Admin>(
            from: PriceOracle.AdminStoragePath
        ) ?? panic("Could not borrow admin resource")
        
        admin.updateFlowPrice(newPrice: newPrice)
    }
}
`, contractAddress)

	tx := flow.NewTransaction().
		SetScript([]byte(script)).
		SetGasLimit(100).
		SetProposalKey(o.account.Address, o.account.Keys[0].Index, o.account.Keys[0].SequenceNumber).
		SetPayer(o.account.Address).
		AddAuthorizer(o.account.Address)

	priceArg, err := CadenceUFix64(price)
	if err != nil {
		return fmt.Errorf("failed to convert price: %w", err)
	}
	if err := tx.AddArgument(priceArg); err != nil {
		return fmt.Errorf("failed to add argument: %w", err)
	}

	if err := tx.SignEnvelope(o.account.Address, o.account.Keys[0].Index, o.signer); err != nil {
		return fmt.Errorf("failed to sign transaction: %w", err)
	}

	if err := o.flowClient.SendTransaction(ctx, *tx); err != nil {
		return fmt.Errorf("failed to send transaction: %w", err)
	}

	result, err := waitForSeal(ctx, o.flowClient, tx.ID())
	if err != nil {
		return fmt.Errorf("failed to wait for seal: %w", err)
	}

	if result.Error != nil {
		return fmt.Errorf("transaction failed: %v", result.Error)
	}

	lastTxID = tx.ID().String()
	log.Printf("✅ Price updated to $%.4f (TX: %s)", price, tx.ID())
	return nil
}

func (o *OracleUpdater) Run() {
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	log.Printf("🚀 Starting FLOW price oracle updater")
	log.Printf("📊 Update interval: %v", updateInterval)
	log.Printf("📍 Contract address: %s", contractAddress)
	log.Printf("🌐 Network: Flow Testnet\n")

	o.updatePrice()

	for range ticker.C {
		o.updatePrice()
	}
}

func (o *OracleUpdater) updatePrice() {

	price, err := o.GetFlowPrice()
	if err != nil {
		log.Printf("❌ Error fetching price: %v", err)
		return
	}

	log.Printf("💰 Fetched FLOW price: $%.4f", price)

	txID := ""
	if err := o.UpdatePriceOnChain(price); err != nil {
		log.Printf("❌ Error updating price on-chain: %v", err)
		return
	} else {

		txID = o.getLastTxID()
	}

	if err := o.SavePriceToDatabase(price, txID); err != nil {
		log.Printf("❌ Error saving price to database: %v", err)

	}
}

func waitForSeal(ctx context.Context, client *grpc.Client, txID flow.Identifier) (*flow.TransactionResult, error) {
	for {
		result, err := client.GetTransactionResult(ctx, txID)
		if err != nil {
			return nil, err
		}

		if result.Status == flow.TransactionStatusSealed {
			return result, nil
		}

		time.Sleep(1 * time.Second)
	}
}

func CadenceUFix64(value float64) (cadence.Value, error) {

	intValue := uint64(value * 100000000)
	return cadence.NewUFix64(fmt.Sprintf("%d.%08d", intValue/100000000, intValue%100000000))
}

func (o *OracleUpdater) getLastTxID() string {
	return lastTxID
}

func (o *OracleUpdater) SavePriceToDatabase(price float64, txHash string) error {
	ctx := context.Background()

	priceDecimal := decimal.NewFromFloat(price)

	id := uuid.New().String()

	query := `
		INSERT INTO price_oracle (id, symbol, price_usd, tx_hash, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`

	_, err := o.db.Exec(ctx, query, id, "FLOW", priceDecimal, txHash, time.Now())
	if err != nil {
		return fmt.Errorf("failed to insert price into database: %w", err)
	}

	log.Printf("💾 Price saved to database (ID: %s)", id)
	return nil
}

func (o *OracleUpdater) Close() error {
	o.db.Close()
	return o.flowClient.Close()
}

func main() {

	privateKey := os.Getenv("FLOW_PRIVATE_KEY")
	if privateKey == "" {
		log.Fatal("FLOW_PRIVATE_KEY environment variable is required")
	}

	accountAddress := os.Getenv("FLOW_ACCOUNT_ADDRESS")
	if accountAddress == "" {
		accountAddress = "0xe3f7e4d39675d8d3"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	updater, err := NewOracleUpdater(privateKey, accountAddress, databaseURL)
	if err != nil {
		log.Fatalf("Failed to create oracle updater: %v", err)
	}
	defer updater.Close()

	updater.Run()
}

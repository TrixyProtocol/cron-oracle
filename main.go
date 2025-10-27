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
	"github.com/joho/godotenv"
	"github.com/onflow/cadence"
	"github.com/onflow/flow-go-sdk"
	"github.com/onflow/flow-go-sdk/client"
	"github.com/onflow/flow-go-sdk/crypto"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	flowAccessNode = "access.testnet.nodes.onflow.org:9000"
	updateInterval = 5 * time.Minute
	coinGeckoAPI   = "https://api.coingecko.com/api/v3/simple/price?ids=flow&vs_currencies=usd"
)

var contractAddress string

type PriceResponse struct {
	Flow struct {
		USD float64 `json:"usd"`
	} `json:"flow"`
}

type OracleUpdater struct {
	flowClient *client.Client
	account    *flow.Account
	privateKey crypto.PrivateKey
	signer     crypto.Signer
	db         *pgxpool.Pool
}

func NewOracleUpdater(privateKeyHex string, accountAddress string, databaseURL string) (*OracleUpdater, error) {
	flowClient, err := client.New(flowAccessNode, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Flow: %w", err)
	}

	privateKey, err := crypto.DecodePrivateKeyHex(crypto.ECDSA_secp256k1, privateKeyHex)
	if err != nil {
		log.Printf("⚠️  Failed to decode with secp256k1, trying P256: %v", err)
		privateKey, err = crypto.DecodePrivateKeyHex(crypto.ECDSA_P256, privateKeyHex)
		if err != nil {
			return nil, fmt.Errorf("failed to decode private key: %w", err)
		}
	}

	hashAlgo := crypto.SHA3_256

	signer, err := crypto.NewInMemorySigner(privateKey, hashAlgo)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	address := flow.HexToAddress(accountAddress)

	err = flowClient.Ping(context.Background())
	if err != nil {
		log.Printf("⚠️  Failed to ping Flow network: %v", err)
	}

	account, err := flowClient.GetAccount(context.Background(), address)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	log.Printf("✅ Flow account loaded: %s (keys: %d)", address.Hex(), len(account.Keys))

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

	account, err := o.flowClient.GetAccount(ctx, o.account.Address)
	if err != nil {
		return fmt.Errorf("failed to get account: %w", err)
	}

	latestBlock, err := o.flowClient.GetLatestBlock(ctx, true)
	if err != nil {
		return fmt.Errorf("failed to get latest block: %w", err)
	}

	tx := flow.NewTransaction().
		SetScript([]byte(script)).
		SetReferenceBlockID(latestBlock.ID).
		SetGasLimit(100).
		SetProposalKey(o.account.Address, o.account.Keys[0].Index, account.Keys[0].SequenceNumber).
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

	skipBlockchain := os.Getenv("SKIP_BLOCKCHAIN") == "true"

	if !skipBlockchain {
		if err := o.UpdatePriceOnChain(price); err != nil {
			log.Printf("❌ Error updating price on-chain: %v", err)
			log.Printf("⚠️  Continuing with database update only...")
			txID = "skipped_" + fmt.Sprintf("%d", time.Now().Unix())
		} else {

			txID = o.getLastTxID()
		}
	} else {
		log.Printf("⚠️  Skipping blockchain update (SKIP_BLOCKCHAIN=true)")
		txID = "local_" + fmt.Sprintf("%d", time.Now().Unix())
	}

	if err := o.SavePriceToDatabase(price, txID); err != nil {
		log.Printf("❌ Error saving price to database: %v", err)
	} else {

		if err := o.UpdateProtocolAPYs(price); err != nil {
			log.Printf("❌ Error updating protocol APYs: %v", err)
		}
	}
}

func waitForSeal(ctx context.Context, c *client.Client, txID flow.Identifier) (*flow.TransactionResult, error) {
	for {
		result, err := c.GetTransactionResult(ctx, txID)
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

var lastPriceOracleID string

func (o *OracleUpdater) SavePriceToDatabase(price float64, txHash string) error {
	ctx := context.Background()

	priceDecimal := decimal.NewFromFloat(price)

	id := uuid.New().String()
	lastPriceOracleID = id

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

func (o *OracleUpdater) UpdateProtocolAPYs(flowPrice float64) error {
	ctx := context.Background()

	baseRates := map[string]float64{
		"ankr":      12.5,
		"increment": 15.3,
		"figment":   10.8,
	}

	for protocol, baseAPY := range baseRates {

		priceImpact := 1.0 + (1.0 - flowPrice)
		adjustedAPY := baseAPY * priceImpact

		if adjustedAPY < 5.0 {
			adjustedAPY = 5.0
		} else if adjustedAPY > 50.0 {
			adjustedAPY = 50.0
		}

		id := uuid.New().String()
		query := `
			INSERT INTO protocol_apy_snapshots 
			(id, protocol_name, apy, flow_price, price_oracle_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`

		_, err := o.db.Exec(ctx, query,
			id,
			protocol,
			decimal.NewFromFloat(adjustedAPY),
			decimal.NewFromFloat(flowPrice),
			lastPriceOracleID,
			time.Now(),
		)

		if err != nil {
			log.Printf("⚠️  Failed to save APY for %s: %v", protocol, err)
			continue
		}

		log.Printf("📈 %s APY: %.2f%% (price impact: %.2fx)", protocol, adjustedAPY, priceImpact)
	}

	return nil
}

func (o *OracleUpdater) Close() error {
	o.db.Close()
	return o.flowClient.Close()
}

func main() {

	if err := godotenv.Load(); err != nil {
		log.Println("⚠️  No .env file found, using system environment variables")
	}

	privateKey := os.Getenv("FLOW_PRIVATE_KEY")
	if privateKey == "" {
		log.Fatal("FLOW_PRIVATE_KEY environment variable is required")
	}

	accountAddress := os.Getenv("FLOW_ACCOUNT_ADDRESS")
	if accountAddress == "" {
		log.Fatal("FLOW_ACCOUNT_ADDRESS environment variable is required")
	}

	contractAddress = os.Getenv("PRICE_ORACLE_CONTRACT")
	if contractAddress == "" {
		contractAddress = accountAddress
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

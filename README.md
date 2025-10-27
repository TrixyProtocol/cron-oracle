# Trixy Protocol - Price Oracle Updater

Automated service that fetches FLOW token prices from CoinGecko and updates both the Flow blockchain PriceOracle contract and the backend PostgreSQL database.

## Features

- ✅ Fetches real-time FLOW/USD price from CoinGecko API
- ✅ Updates price on Flow blockchain (testnet) every 5 minutes
- ✅ Stores historical price data in PostgreSQL database
- ✅ Automatic retry and error handling
- ✅ Comprehensive logging

## Architecture

```
┌─────────────────┐
│  CoinGecko API  │ ← Fetch FLOW price
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Price Oracle   │
│    Updater      │
└────┬───────┬────┘
     │       │
     │       └──────────────┐
     ▼                      ▼
┌──────────────┐    ┌──────────────┐
│   Flow       │    │  PostgreSQL  │
│  Blockchain  │    │   Database   │
│  (Testnet)   │    │  (Backend)   │
└──────────────┘    └──────────────┘
```

## Prerequisites

- Go 1.21 or later
- PostgreSQL database (shared with backend)
- Flow blockchain account with PriceOracle admin access

## Installation

1. **Clone and navigate to the directory**
   ```bash
   cd cron-oracle
   ```

2. **Install dependencies**
   ```bash
   go mod tidy
   ```

3. **Run the database migration**
   ```bash
   psql $DATABASE_URL -f ../backend/migrations/007_price_oracle.sql
   ```

4. **Configure environment variables**
   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   ```

## Configuration

Create a `.env` file with the following variables:

```bash
# Database Configuration
DATABASE_URL=postgresql://user:password@localhost:5432/trixy-flow-indexer

# Flow Blockchain Configuration
FLOW_PRIVATE_KEY=your_private_key_here
FLOW_ACCOUNT_ADDRESS=0xe3f7e4d39675d8d3
```

### Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `DATABASE_URL` | PostgreSQL connection string | ✅ Yes |
| `FLOW_PRIVATE_KEY` | Private key for Flow account with admin access | ✅ Yes |
| `FLOW_ACCOUNT_ADDRESS` | Flow account address (defaults to contract address) | ⚠️ Optional |

## Usage

### Run with Script (Recommended)

```bash
./run.sh
```

This automatically loads `.env` and starts the service.

### Run Manually

```bash
export $(cat .env | grep -v '^#' | xargs)
go run main.go
```

### Build and Run

```bash
go build -o oracle main.go
./oracle
```

## How It Works

1. **Price Fetching**: Every 5 minutes, fetches current FLOW/USD price from CoinGecko
2. **Blockchain Update**: Sends transaction to update PriceOracle contract on Flow testnet
3. **Database Storage**: Saves price data with transaction hash to PostgreSQL
4. **Logging**: Logs all operations (success/failure) to stdout

### Update Cycle

```
┌─────────────────────────────────────────┐
│  Fetch Price from CoinGecko             │
│  (e.g., $0.2767)                        │
└────────────┬────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────┐
│  Update Flow Blockchain                 │
│  - Create transaction                   │
│  - Sign with private key                │
│  - Wait for seal                        │
└────────────┬────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────┐
│  Save to Database                       │
│  - Insert price with tx hash            │
│  - Log success                          │
└─────────────────────────────────────────┘
             │
             ▼
        Wait 5 minutes ⏱️
             │
             └──────────────┐
                            │
                            ▼
                    Repeat cycle...
```

## Database Schema

The service uses the `price_oracle` table:

```sql
CREATE TABLE price_oracle (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol VARCHAR(20) NOT NULL,
    price_usd DECIMAL(20, 8) NOT NULL,
    tx_hash TEXT,
    block_number BIGINT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);
```

### Query Examples

**Get latest price:**
```sql
SELECT * FROM price_oracle 
WHERE symbol = 'FLOW' 
ORDER BY created_at DESC 
LIMIT 1;
```

**Get price history (24 hours):**
```sql
SELECT * FROM price_oracle 
WHERE symbol = 'FLOW' 
AND created_at > NOW() - INTERVAL '24 hours'
ORDER BY created_at DESC;
```

**Calculate average price:**
```sql
SELECT 
    DATE_TRUNC('hour', created_at) as hour,
    AVG(price_usd) as avg_price,
    MIN(price_usd) as min_price,
    MAX(price_usd) as max_price
FROM price_oracle
WHERE symbol = 'FLOW'
GROUP BY hour
ORDER BY hour DESC;
```

## Testing

### Test Database Connection

```bash
./test_db.sh
```

This verifies:
- ✅ Database table exists
- ✅ Can insert records
- ✅ Can query records
- ✅ Indexes are working

### Verify Price Fetching

```bash
curl -s "https://api.coingecko.com/api/v3/simple/price?ids=flow&vs_currencies=usd" | python3 -m json.tool
```

## Logs

The service outputs structured logs:

```
🚀 Starting FLOW price oracle updater
📊 Update interval: 5m0s
📍 Contract address: 0xe3f7e4d39675d8d3
🌐 Network: Flow Testnet

✅ Database connection established
💰 Fetched FLOW price: $0.2767
✅ Price updated to $0.2767 (TX: abc123...)
💾 Price saved to database (ID: def456...)
```

### Log Symbols

- 🚀 Service started
- 💰 Price fetched
- ✅ Operation successful
- ❌ Error occurred
- 💾 Database operation
- 📊 Status information

## Deployment

### Production Considerations

1. **Use systemd service** (Linux)
   ```ini
   [Unit]
   Description=Trixy Price Oracle
   After=network.target postgresql.service

   [Service]
   Type=simple
   User=trixy
   WorkingDirectory=/opt/trixy/cron-oracle
   EnvironmentFile=/opt/trixy/cron-oracle/.env
   ExecStart=/opt/trixy/cron-oracle/oracle
   Restart=always
   RestartSec=10

   [Install]
   WantedBy=multi-user.target
   ```

2. **Monitor logs**
   ```bash
   journalctl -u trixy-oracle -f
   ```

3. **Set up alerts** for:
   - Price fetch failures
   - Blockchain transaction failures
   - Database connection issues

## Troubleshooting

### Common Issues

**1. "failed to connect to Flow"**
- Check internet connection
- Verify Flow testnet is accessible: `access.devnet.nodes.onflow.org:9000`

**2. "failed to decode private key"**
- Ensure `FLOW_PRIVATE_KEY` is valid hex string
- Check key format (should be 64 hex characters)

**3. "failed to connect to database"**
- Verify PostgreSQL is running
- Check `DATABASE_URL` format
- Ensure database exists and migrations are run

**4. "transaction failed: Could not borrow admin resource"**
- Account doesn't have admin access to PriceOracle contract
- Verify correct account address and private key

**5. "Error fetching price"**
- CoinGecko API might be rate-limited
- Check internet connection
- Verify API endpoint is accessible

## Files

```
cron-oracle/
├── main.go              # Main application code
├── go.mod               # Go dependencies
├── go.sum               # Dependency checksums
├── .env                 # Environment configuration (gitignored)
├── .env.example         # Environment template
├── .gitignore           # Git ignore rules
├── run.sh               # Startup script
├── test_db.sh           # Database test script
├── README.md            # This file
├── README_DB.md         # Database integration docs
└── SETUP_COMPLETE.md    # Setup summary
```

## API Reference

### CoinGecko API

**Endpoint:**
```
GET https://api.coingecko.com/api/v3/simple/price?ids=flow&vs_currencies=usd
```

**Response:**
```json
{
  "flow": {
    "usd": 0.276873
  }
}
```

### Flow PriceOracle Contract

**Contract Address:** `0xe3f7e4d39675d8d3` (Testnet)

**Transaction Script:**
```cadence
import PriceOracle from 0xe3f7e4d39675d8d3

transaction(newPrice: UFix64) {
    prepare(signer: auth(Storage) &Account) {
        let admin = signer.storage.borrow<&PriceOracle.Admin>(
            from: PriceOracle.AdminStoragePath
        ) ?? panic("Could not borrow admin resource")
        
        admin.updateFlowPrice(newPrice: newPrice)
    }
}
```

## Contributing

When contributing, ensure:
1. Code compiles: `go build`
2. Tests pass: `./test_db.sh`
3. Environment variables are documented
4. Logs are clear and helpful

## License

Part of the Trixy Protocol project.

## Support

For issues or questions:
1. Check the troubleshooting section
2. Review logs for error messages
3. Verify all environment variables are set correctly
4. Test database connection with `./test_db.sh`

---

**Note:** This service is designed for Flow testnet. For mainnet deployment, update the `flowAccessNode` constant and contract address in `main.go`.

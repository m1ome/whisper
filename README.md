# Ethereum event listener webhook tool
> Simple, low-memory footprint, efficient

## Basic configuration
```bash
Usage of /app/whisper:
  -a string
    	address for smart contract to watch events from (default "abi.json")
  -c int
    	chunk of blocks to parse in one run (default 100)
  -d int
    	delay in seconds before each run (default 10)
  -db string
    	database to store information of parsed blocks (default "block.txt")
  -e string
    	ethereum api endpoint
  -live string
    	liveness endpoint to bind on (default ":9000")
  -s int
    	starting block
  -t string
    	topic to parse
  -w string
    	webhook enpoint to send events to
```

## Docker usage
```bash
docker pull w1n2k/whisper:latest
docker run whisper:latest
```

## Webhooks
**Method:** POST\
**Body:**
```json
{
  "event": "Transfer",
  "tx_hash": "0x029cda1ea895b98ed79ab9e73b093347b6d0077fe7ad89727efdca881190e56f",
  "index": 0,
  "data": {
    "from": "0x51b80eed094adbfddad6624596cea430c0f543fa",
    "to": "0x21e3013f810b72f317ddaec8ffa371b8e1762e22",
    "value": 1e+25
  }
}
```

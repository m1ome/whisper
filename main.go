package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/heptiolabs/healthcheck"
)

var (
	abiFile          string
	topicName        string
	endpoint         string
	address          string
	webhook          string
	db               string
	livenessEndpoint string
	delay            int64
	chunkSize        int64
	startingBlock    int64
)

func init() {
	flag.StringVar(&topicName, "t", "", "topic to parse")
	flag.StringVar(&endpoint, "e", "", "ethereum api endpoint")
	flag.StringVar(&address, "a", "", "address for smart contract to watch events from")
	flag.StringVar(&abiFile, "abi", "abi.json", "abi json file")
	flag.StringVar(&webhook, "w", "", "webhook enpoint to send events to")
	flag.StringVar(&livenessEndpoint, "live", ":9000", "liveness endpoint to bind on")
	flag.StringVar(&db, "db", "block.txt", "database to store information of parsed blocks")
	flag.Int64Var(&delay, "d", 10, "delay in seconds before each run")
	flag.Int64Var(&chunkSize, "c", 100, "chunk of blocks to parse in one run")
	flag.Int64Var(&startingBlock, "s", 0, "starting block")
	flag.Parse()
}

type WebhookRequest struct {
	Event  string         `json:"event"`
	TxHash string         `json:"tx_hash"`
	Index  int64          `json:"index"`
	Data   map[string]any `json:"data"`
}

func main() {
	if topicName == "" {
		log.Fatal("error: please specify a topic to parse")
	}
	if endpoint == "" {
		log.Fatal("error: please specify enpoint to work with")
	}
	if address == "" {
		log.Fatal("error: please specify address")
	}
	if webhook == "" {
		log.Fatal("error: please specify a webhook endpoint")
	}

	body, err := os.ReadFile(db)
	if err == nil && string(body) != "" {
		i, err := strconv.ParseInt(string(body), 10, 64)
		if err != nil {
			log.Print("db: error corruped last block in block file, used default one")
		} else if i > 0 {
			log.Printf("db: found %d block in block file, using it", i)
			startingBlock = i
		}
	}

	// Reading a first block to start from
	if _, err := os.OpenFile(db, os.O_CREATE|os.O_RDWR, 0755); err != nil {
		log.Fatalf("error opening/creating database: %v", err)
	}

	client, err := ethclient.Dial(endpoint)
	if err != nil {
		log.Fatalf("error dialing ethereum client: %v", err)
	}
	defer client.Close()

	f, err := os.Open(abiFile)
	if err != nil {
		log.Fatalf("error opening abi file: %v", err)
	}
	contractAbi, err := abi.JSON(f)
	if err != nil {
		log.Fatalf("error reading abi: %v", err)
	}

	// Finding out a particular event
	var mappedEvent abi.Event
	indexed := make([]abi.Argument, 0)
	for _, event := range contractAbi.Events {
		if event.Name != topicName {
			continue
		}

		for _, input := range event.Inputs {
			if input.Indexed {
				indexed = append(indexed, input)
			}
		}

		mappedEvent = event
	}

	log.Printf("starting to work with contract %s and event %s at block %d", address, topicName, startingBlock)
	ticker := time.NewTicker(time.Second * time.Duration(delay))
	currentBlock := startingBlock

	// Starting readiness probe
	health := healthcheck.NewHandler()
	go http.ListenAndServe(livenessEndpoint, health)
	log.Printf("start listening for liveness checks on %s", livenessEndpoint)

	for {
		select {
		case <-ticker.C:
			header, err := client.HeaderByNumber(context.Background(), nil)
			if err != nil {
				log.Printf("error getting header: %v", err)
				break
			}
			lastBlock := header.Number.Int64()
			toBlock := currentBlock + chunkSize
			if lastBlock < toBlock {
				toBlock = lastBlock
			}

			log.Printf("parsing events from %d to %d", currentBlock, currentBlock+chunkSize)
			query := ethereum.FilterQuery{
				FromBlock: big.NewInt(currentBlock),
				ToBlock:   big.NewInt(toBlock),
				Addresses: []common.Address{
					common.HexToAddress(address),
				},
			}

			logs, err := client.FilterLogs(context.Background(), query)
			if err != nil {
				log.Printf("error filtering logs: %v", err)
				break
			}

			if err := parseLogs(logs, mappedEvent, indexed, contractAbi); err != nil {
				log.Printf("error parsing logs: %v", err)
				break
			}

			if err := os.WriteFile(db, []byte(fmt.Sprintf("%d", currentBlock+chunkSize)), 0755); err != nil {
				log.Fatalf("error writing last block to database: %v", err)
			}

			currentBlock += chunkSize
		}
	}
}

func parseLogs(logs []types.Log, mappedEvent abi.Event, indexed []abi.Argument, contractAbi abi.ABI) error {
	httpClient := &http.Client{
		Timeout: time.Second * 30,
	}

	for _, l := range logs {
		if l.Topics[0] == mappedEvent.ID {
			currentEvent := map[string]any{}
			if err := abi.ParseTopicsIntoMap(currentEvent, indexed, l.Topics[1:]); err != nil {
				return fmt.Errorf("error parsing indexed topics: %v", err)
			}
			if err := contractAbi.UnpackIntoMap(currentEvent, mappedEvent.Name, l.Data); err != nil {
				return fmt.Errorf("error parsing data: %v", err)
			}

			webhookEvent := WebhookRequest{
				TxHash: l.TxHash.Hex(),
				Index:  int64(l.Index),
				Event:  mappedEvent.Name,
				Data:   currentEvent,
			}

			body, err := json.Marshal(webhookEvent)
			if err != nil {
				return fmt.Errorf("error encoding event: %v", err)
			}

			req, err := http.NewRequest("POST", webhook, bytes.NewBuffer(body))
			if err != nil {
				return fmt.Errorf("error creating request: %v", err)
			}
			defer req.Body.Close()

			res, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("error sending request: %v", err)
			}

			if res.StatusCode > 299 || res.StatusCode < 200 {
				return fmt.Errorf("error in response code %d", res.StatusCode)
			}

			log.Printf("found event at tx %s, with params: %v", webhookEvent.TxHash, webhookEvent)
		}
	}

	return nil
}

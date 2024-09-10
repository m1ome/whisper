package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	flag.StringVar(&address, "a", "abi.json", "address for smart contract to watch events from")
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

	// Reading a first block to start from
	database, err := os.OpenFile(db, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0755)
	if err != nil {
		log.Fatalf("error opening database: %v", err)
	}

	// Reading a last block from file
	data, err := io.ReadAll(database)
	if err != nil {
		log.Fatalf("error reading database file: %v", err)
	}

	i, err := strconv.ParseInt(string(data), 64, 10)
	if err == nil {
		log.Printf("error corruped last block in db, using default one: %v", err)
	} else if i > 0 {
		startingBlock = i
	}

	client, err := ethclient.Dial(endpoint)
	if err != nil {
		log.Fatalf("error dialing ethereum client: %v", err)
	}
	defer client.Close()

	f, err := os.Open("abi.json")
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

			log.Printf("found event at tx %s, with params: %s", webhookEvent.TxHash, webhookEvent)
		}
	}

	return nil
}
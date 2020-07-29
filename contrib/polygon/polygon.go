package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alpacahq/marketstore/v4/contrib/polygon/api"
	"github.com/alpacahq/marketstore/v4/contrib/polygon/backfill"
	"github.com/alpacahq/marketstore/v4/contrib/polygon/handlers"
	"github.com/alpacahq/marketstore/v4/contrib/polygon/streaming"
	"github.com/alpacahq/marketstore/v4/executor"
	"github.com/alpacahq/marketstore/v4/planner"
	"github.com/alpacahq/marketstore/v4/plugins/bgworker"
	"github.com/alpacahq/marketstore/v4/utils"
	"github.com/alpacahq/marketstore/v4/utils/io"
	"github.com/alpacahq/marketstore/v4/utils/log"
)

type PolygonFetcher struct {
	config FetcherConfig
	types  map[string]struct{} // Bars, Quotes, Trades
}

type FetcherConfig struct {
	// AddTickCountToBars controls if TickCnt is added to the schema for Bars or not
	AddTickCountToBars bool `json:"add_bar_tick_count,omitempty"`
	// polygon API key for authenticating with their APIs
	APIKey string `json:"api_key"`
	// polygon API base URL in case it is being proxied
	// (defaults to https://api.polygon.io/)
	BaseURL string `json:"base_url"`
	// websocket servers for Polygon, default is: "wss://socket.polygon.io"
	WSServers string `json:"ws_servers"`
	// list of data types to subscribe to (one of bars, quotes, trades)
	DataTypes []string `json:"data_types"`
	// list of symbols that are important
	Symbols []string `json:"symbols"`
	// time string when to start first time, in "YYYY-MM-DD HH:MM" format
	// if it is restarting, the start is the last written data timestamp
	// otherwise, it starts from the latest streamed bar
	QueryStart string `json:"query_start"`
}

var (
	minute = utils.NewTimeframe("1Min")
)

// NewBgWorker returns a new instances of PolygonFetcher. See FetcherConfig
// for more details about configuring PolygonFetcher.
func NewBgWorker(conf map[string]interface{}) (w bgworker.BgWorker, err error) {
	data, _ := json.Marshal(conf)
	config := FetcherConfig{}
	err = json.Unmarshal(data, &config)
	if err != nil {
		return
	}

	t := map[string]struct{}{}

	for _, dt := range config.DataTypes {
		if dt == "bars" || dt == "quotes" || dt == "trades" {
			t[dt] = struct{}{}
		}
	}

	if len(t) == 0 {
		return nil, fmt.Errorf("at least one valid data_type is required")
	}

	backfill.BackfillM = &sync.Map{}

	return &PolygonFetcher{
		config: config,
		types:  t,
	}, nil
}

// Run the PolygonFetcher. It starts the streaming API as well as the
// asynchronous backfilling routine.
func (pf *PolygonFetcher) Run() {
	api.SetAPIKey(pf.config.APIKey)

	if pf.config.BaseURL != "" {
		api.SetBaseURL(pf.config.BaseURL)
	}

	var subscription []string
	for t := range pf.types {
		switch t {
		case "bars":
			subscription = append(subscription, "AM.*")
		case "quotes":
			subscription = append(subscription, "Q.*")
		case "trades":
			subscription = append(subscription, "T.*")
		}
	}

	ws := streaming.NewClient(pf.config.WSServers+"/stocks", pf.config.APIKey, strings.Join(subscription, ","))
	ws.TradeHandler = handlers.TradeHandler
	ws.QuoteHandler = handlers.QuoteHandler
	ws.AggregateHandler = handlers.BarsHandlerWrapper(pf.config.AddTickCountToBars)
	ws.Listen(context.Background())
}

func (pf *PolygonFetcher) workBackfillBars() {
	ticker := time.NewTicker(30 * time.Second)

	for range ticker.C {
		wg := sync.WaitGroup{}
		count := 0

		// range over symbols that need backfilling, and
		// backfill them from the last written record
		backfill.BackfillM.Range(func(key, value interface{}) bool {
			symbol := key.(string)
			// make sure epoch value isn't nil (i.e. hasn't
			// been backfilled already)
			if value != nil {
				go func() {
					wg.Add(1)
					defer wg.Done()

					// backfill the symbol in parallel
					pf.backfillBars(symbol, time.Unix(*value.(*int64), 0))
					backfill.BackfillM.Store(key, nil)
				}()
			}

			// limit 10 goroutines per CPU core
			if count >= runtime.NumCPU()*10 {
				return false
			}

			return true
		})
		wg.Wait()
	}
}

func (pf *PolygonFetcher) backfillBars(symbol string, end time.Time) {
	var (
		from time.Time
		err  error
		tbk  = io.NewTimeBucketKey(fmt.Sprintf("%s/1Min/OHLCV", symbol))
	)

	// query the latest entry prior to the streamed record
	if pf.config.QueryStart == "" {
		instance := executor.ThisInstance
		cDir := instance.CatalogDir
		q := planner.NewQuery(cDir)
		q.AddTargetKey(tbk)
		q.SetRowLimit(io.LAST, 1)
		q.SetEnd(end.Add(-1 * time.Minute))

		parsed, err := q.Parse()
		if err != nil {
			log.Error("[polygon] query parse failure (%v)", err)
			return
		}

		scanner, err := executor.NewReader(parsed)
		if err != nil {
			log.Error("[polygon] new scanner failure (%v)", err)
			return
		}

		csm, err := scanner.Read()
		if err != nil {
			log.Error("[polygon] scanner read failure (%v)", err)
			return
		}

		epoch := csm[*tbk].GetEpoch()

		// no gap to fill
		if len(epoch) == 0 {
			return
		}

		from = time.Unix(epoch[len(epoch)-1], 0)

	} else {
		for _, layout := range []string{
			"2006-01-02 03:04:05",
			"2006-01-02T03:04:05",
			"2006-01-02 03:04",
			"2006-01-02T03:04",
			"2006-01-02",
		} {
			from, err = time.Parse(layout, pf.config.QueryStart)
			if err == nil {
				break
			}
		}
	}

	// request & write the missing bars
	if err = backfill.Bars(symbol, from, time.Time{}); err != nil {
		log.Error("[polygon] bars backfill failure for key: [%v] (%v)", tbk.String(), err)
	}
}

func main() {}

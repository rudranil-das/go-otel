package viewstate

import (
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/aggregator"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/histogram"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/lastvalue"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/sum"
	"go.opentelemetry.io/otel/sdk/metric/export/aggregation"
	"go.opentelemetry.io/otel/sdk/metric/number"
	"go.opentelemetry.io/otel/sdk/metric/number/traits"
	"go.opentelemetry.io/otel/sdk/metric/reader"
	"go.opentelemetry.io/otel/sdk/metric/sdkapi"
	"go.opentelemetry.io/otel/sdk/metric/views"
)

type (
	Compiler struct {
		library instrumentation.Library
		views   []views.View
		readers []*reader.Reader
	}

	Instrument interface {
		NewCollector(kvs []attribute.KeyValue) Collector
	}

	Collector interface {
		Collect()
	}

	Updater[N number.Any] interface {
		Update(value N)
	}

	CollectorUpdater[N number.Any] interface {
		Collector
		Updater[N]
	}

	multiCollector[N number.Any] struct {
		collectors []Collector
	}

	multiInstrument[N number.Any] struct {
		compiled []Instrument
	}

	configuredBehavior struct {
		desc     sdkapi.Descriptor
		reader   *reader.Reader
		view     views.View
		settings aggregatorSettings
		metric   *viewMetric
	}

	aggregatorSettings struct {
		kind  aggregation.Kind
		hcfg  histogram.Config
		scfg  sum.Config
		lvcfg lastvalue.Config
	}

	viewMetric struct {
		desc sdkapi.Descriptor
	}

	syncCollector[N number.Any, Storage, Config any, Methods aggregator.Methods[N, Storage, Config]] struct {
		current  Storage
		snapshot Storage
		output   *Storage
	}

	asyncCollector[N number.Any, Storage, Config any, Methods aggregator.Methods[N, Storage, Config]] struct {
		lock     sync.Mutex
		current  N
		snapshot Storage
		output   *Storage
	}

	compiledSyncView[N number.Any, Storage, Config any, Methods aggregator.Methods[N, Storage, Config]] struct {
		metric    *viewMetric
		aggConfig *Config
		viewKeys  attribute.Filter
	}

	compiledAsyncView[N number.Any, Storage, Config any, Methods aggregator.Methods[N, Storage, Config]] struct {
		metric    *viewMetric
		aggConfig *Config
		viewKeys  attribute.Filter
	}
)

func aggregatorSettingsFor(desc sdkapi.Descriptor, defaults reader.DefaultsFunc) aggregatorSettings {
	aggr, _ := defaults(desc.InstrumentKind())
	return aggregatorSettings{
		kind: aggr,
	}
}

func viewDescriptor(instrument sdkapi.Descriptor, v views.View) sdkapi.Descriptor {
	ikind := instrument.InstrumentKind()
	nkind := instrument.NumberKind()
	name := instrument.Name()
	description := instrument.Description()
	unit := instrument.Unit()
	if v.HasName() {
		name = v.Name()
	}
	if v.Description() != "" {
		description = instrument.Description()
	}
	return sdkapi.NewDescriptor(name, ikind, nkind, description, unit)
}

func New(lib instrumentation.Library, views []views.View, readers []*reader.Reader) *Compiler {

	// TODO: error checking here, such as:
	// - empty (?)
	// - duplicate name
	// - invalid inst/number/aggregation kind
	// - both instrument name and regexp
	// - schemaURL or Version without library name
	// - empty attribute keys
	// - Name w/o SingleInst
	return &Compiler{
		library: lib,
		readers: readers,
	}
}

// Compile is called during NewInstrument by the Meter
// implementation, the result saved in the instrument and used to
// construct new Collectors throughout its lifetime.
func (v *Compiler) Compile(instrument sdkapi.Descriptor) Instrument {
	var configs []configuredBehavior

	for _, reader := range v.readers {
		matchCount := 0
		for _, view := range v.views {
			if !view.Matches(v.library, instrument) {
				continue
			}
			matchCount++
			var as aggregatorSettings
			switch view.Aggregation() {
			case aggregation.SumKind, aggregation.LastValueKind:
				// These have no options
				as.kind = view.Aggregation()
			case aggregation.HistogramKind:
				as.kind = view.Aggregation()
				as.hcfg = histogram.NewConfig(
					histogramDefaultsFor(instrument.NumberKind()),
					view.HistogramOptions()...,
				)
			default:
				as = aggregatorSettingsFor(instrument, reader.Defaults())
			}

			if as.kind == aggregation.DropKind {
				continue
			}

			configs = append(configs, configuredBehavior{
				desc:     instrument,
				reader:   reader,
				view:     view,
				settings: as,
			})
		}

		// If there were no matching views, set the default aggregation.
		if matchCount == 0 {
			as := aggregatorSettingsFor(instrument, reader.Defaults())
			if as.kind == aggregation.DropKind {
				continue
			}

			configs = append(configs, configuredBehavior{
				desc:     instrument,
				reader:   reader,
				view:     views.New(views.WithAggregation(as.kind)),
				settings: as,
			})
		}
	}
	// When there are no matches for any terminal, return a nil factory.
	if len(configs) == 0 {
		return nil
	}

	var compiled []Instrument

	for _, config := range configs {
		viewDesc := viewDescriptor(config.desc, config.view)

		if available := config.reader.AcquireNameCheck(viewDesc.Name()); !available {
			otel.Handle(fmt.Errorf("duplicate view name registered"))
			continue
		}
		config.metric = &viewMetric{
			desc: viewDesc,
		}

		var one Instrument
		switch viewDesc.NumberKind() {
		case number.Int64Kind:
			one = buildView[int64, traits.Int64](config)
		case number.Float64Kind:
			one = buildView[float64, traits.Float64](config)
		}
		compiled = append(compiled, one)
	}

	switch len(compiled) {
	case 0:
		return nil
	case 1:
		return compiled[0]
	}
	switch instrument.NumberKind() {
	case number.Int64Kind:
		return &multiInstrument[int64]{
			compiled: compiled,
		}
	case number.Float64Kind:
		return &multiInstrument[float64]{
			compiled: compiled,
		}
	}
	return nil
}

func histogramDefaultsFor(kind number.Kind) histogram.Defaults {
	if kind == number.Int64Kind {
		return histogram.Int64Defaults{}
	}
	return histogram.Float64Defaults{}
}

func buildView[N number.Any, Traits traits.Any[N]](config configuredBehavior) Instrument {
	if config.metric.desc.InstrumentKind().Synchronous() {
		return compileSync[N, Traits](config)
	}
	return compileAsync[N, Traits](config)
}

func newSyncInstrument[
	N number.Any,
	Storage, Config any,
	Methods aggregator.Methods[N, Storage, Config],
](config configuredBehavior, aggConfig *Config) Instrument {
	return &compiledSyncView[N, Storage, Config, Methods]{
		metric:    config.metric,
		aggConfig: aggConfig,
		viewKeys:  config.view.Keys(),
	}
}

func (csv *compiledSyncView[N, Storage, Config, Methods]) NewCollector(kvs []attribute.KeyValue) Collector {
	sc := &syncCollector[N, Storage, Config, Methods]{}
	sc.init(csv.metric, *csv.aggConfig, csv.viewKeys, kvs)
	return sc
}

func newAsyncConfig[
	N number.Any,
	Storage, Config any,
	Methods aggregator.Methods[N, Storage, Config],
](config configuredBehavior, aggConfig *Config) Instrument {
	return &compiledAsyncView[N, Storage, Config, Methods]{
		metric:    config.metric,
		aggConfig: aggConfig,
		viewKeys:  config.view.Keys(),
	}
}

func (cav *compiledAsyncView[N, Storage, Config, Methods]) NewCollector(kvs []attribute.KeyValue) Collector {
	sc := &asyncCollector[N, Storage, Config, Methods]{}
	sc.init(cav.metric, *cav.aggConfig, cav.viewKeys, kvs)
	return sc
}

func compileSync[N number.Any, Traits traits.Any[N]](config configuredBehavior) Instrument {
	switch config.settings.kind {
	case aggregation.LastValueKind:
		return newSyncInstrument[
			N,
			lastvalue.State[N, Traits],
			lastvalue.Config,
			lastvalue.Methods[N, Traits, lastvalue.State[N, Traits]],
		](config, &config.settings.lvcfg)
	case aggregation.HistogramKind:
		return newSyncInstrument[
			N,
			histogram.State[N, Traits],
			histogram.Config,
			histogram.Methods[N, Traits, histogram.State[N, Traits]],
		](config, &config.settings.hcfg)
	default:
		return newSyncInstrument[
			N,
			sum.State[N, Traits],
			sum.Config,
			sum.Methods[N, Traits, sum.State[N, Traits]],
		](config, &config.settings.scfg)
	}
}

func compileAsync[N number.Any, Traits traits.Any[N]](config configuredBehavior) Instrument {
	switch config.settings.kind {
	case aggregation.LastValueKind:
		return newAsyncConfig[
			N,
			lastvalue.State[N, Traits],
			lastvalue.Config,
			lastvalue.Methods[N, Traits, lastvalue.State[N, Traits]],
		](config, &config.settings.lvcfg)
	case aggregation.HistogramKind:
		return newAsyncConfig[
			N,
			histogram.State[N, Traits],
			histogram.Config,
			histogram.Methods[N, Traits, histogram.State[N, Traits]],
		](config, &config.settings.hcfg)
	default:
		return newAsyncConfig[
			N,
			sum.State[N, Traits],
			sum.Config,
			sum.Methods[N, Traits, sum.State[N, Traits]],
		](config, &config.settings.scfg)
	}
}

// NewCollector returns a Collector that also implements Updater of the correct type.
func (mi multiInstrument[N]) NewCollector(kvs []attribute.KeyValue) Collector {
	collectors := make([]Collector, 0, len(mi.compiled))
	for _, inst := range mi.compiled {
		collectors = append(collectors, inst.NewCollector(kvs))
	}
	return &multiCollector[N]{
		collectors: collectors,
	}
}

func (c *multiCollector[N]) Collect() {
	for _, coll := range c.collectors {
		coll.Collect()
	}
}

func (c *multiCollector[N]) Update(value N) {
	for _, coll := range c.collectors {
		coll.(Updater[N]).Update(value)
	}
}

func (sc *syncCollector[N, Storage, Config, Methods]) init(metric *viewMetric, cfg Config, keys attribute.Filter, kvs []attribute.KeyValue) {
	var methods Methods
	methods.Init(&sc.current, cfg)
	methods.Init(&sc.snapshot, cfg)

	set, _ := attribute.NewSetWithFiltered(kvs, keys)

	// @@@

	ns := new(Storage)
	methods.Init(ns, cfg)
}

func (sc *syncCollector[N, Storage, Config, Methods]) Update(number N) {
	var methods Methods
	methods.Update(&sc.current, number)
}

func (sc *syncCollector[N, Storage, Config, Methods]) Collect() {
	var methods Methods
	methods.SynchronizedMove(&sc.current, &sc.snapshot)
	methods.Merge(&sc.snapshot, sc.output)
}

func (ac *asyncCollector[N, Storage, Config, Methods]) init(metric *viewMetric, cfg Config, keys attribute.Filter, kvs []attribute.KeyValue) {
	var methods Methods
	ac.current = 0
	methods.Init(&ac.snapshot, cfg)

	set, _ := attribute.NewSetWithFiltered(kvs, keys)

	// @@@ HERE
}

func (ac *asyncCollector[N, Storage, Config, Methods]) Update(number N) {
	ac.lock.Lock()
	defer ac.lock.Unlock()
	ac.current = number
}

func (ac *asyncCollector[N, Storage, Config, Methods]) Collect() {
	ac.lock.Lock()
	defer ac.lock.Unlock()

	var methods Methods
	methods.SynchronizedMove(&ac.snapshot, nil)
	methods.Update(&ac.snapshot, ac.current)
	ac.current = 0

	methods.Merge(&ac.snapshot, ac.output)
}

// func findOutput[N number.Any, Storage, Config any, Methods aggregator.Methods[N, Storage, Config]](metric *viewMetric, aggConfig *Config, viewKeys attribute.Filter, kvs []attribute.KeyValue) *Storage {
// 	set, _ := attribute.NewSetWithFiltered(kvs, viewKeys)
// 	return findOrCreate[N, Storage, Config, Methods](
// 		metric,
// 		set,
// 		aggConfig,
// 	)
// }
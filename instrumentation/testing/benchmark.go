package testing

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentracing/opentracing-go"

	"go.undefinedlabs.com/scopeagent/ast"
	"go.undefinedlabs.com/scopeagent/instrumentation"
	"go.undefinedlabs.com/scopeagent/reflection"
	"go.undefinedlabs.com/scopeagent/runner"
)

type (
	Benchmark struct {
		b *testing.B
	}
)

var (
	benchmarkMapMutex  sync.RWMutex
	benchmarkMap       = map[*testing.B]*Benchmark{}
	benchmarkNameRegex = regexp.MustCompile(`([\w-_:!@#\$%&()=]*)(\/\*\&\/)?`)
)

// Starts a new benchmark using a pc as caller
func StartBenchmark(b *testing.B, pc uintptr, benchFunc func(b *testing.B)) {
	if !hasBenchmark(b) {
		// If the current benchmark is not instrumented, we instrument it.
		startBenchmark(b, pc, benchFunc)
	} else {
		// If the benchmark is already instrumented, we passthrough to the benchFunc
		benchFunc(b)
	}
}

// Runs an auto instrumented sub benchmark
func (bench *Benchmark) Run(name string, f func(b *testing.B)) bool {
	pc, _, _, _ := runtime.Caller(1)
	return bench.b.Run(name, func(innerB *testing.B) {
		startBenchmark(innerB, pc, f)
	})
}

// Adds a benchmark struct to the map
func addBenchmark(b *testing.B, value *Benchmark) {
	benchmarkMapMutex.Lock()
	defer benchmarkMapMutex.Unlock()
	benchmarkMap[b] = value
}

// Gets if the benchmark struct exist
func hasBenchmark(b *testing.B) bool {
	benchmarkMapMutex.RLock()
	defer benchmarkMapMutex.RUnlock()
	_, ok := benchmarkMap[b]
	return ok
}

// Gets the Benchmark struct from *testing.Benchmark
func GetBenchmark(b *testing.B) *Benchmark {
	benchmarkMapMutex.RLock()
	defer benchmarkMapMutex.RUnlock()
	if bench, ok := benchmarkMap[b]; ok {
		return bench
	}
	return nil
}

func startBenchmark(b *testing.B, pc uintptr, benchFunc func(b *testing.B)) {
	var bChild *testing.B
	b.ReportAllocs()
	b.ResetTimer()
	startTime := time.Now()
	result := b.Run("*&", func(b1 *testing.B) {
		addBenchmark(b1, &Benchmark{b: b1})
		benchFunc(b1)
		bChild = b1
	})
	if bChild == nil {
		return
	}
	if getBenchmarkHasSub(bChild) > 0 {
		return
	}
	results, err := extractBenchmarkResult(bChild)
	if err != nil {
		instrumentation.Logger().Printf("Error while extracting the benchmark result object: %v\n", err)
		return
	}

	// Extracting the benchmark func name (by removing any possible sub-benchmark suffix `{bench_func}/{sub_benchmark}`)
	// to search the func source code bounds and to calculate the package name.
	fullTestName := runner.GetOriginalTestName(b.Name())

	// We detect if the parent benchmark is instrumented, and if so we remove the "*" SubBenchmark from the previous instrumentation
	parentBenchmark := getParentBenchmark(b)
	if parentBenchmark != nil && hasBenchmark(parentBenchmark) {
		var nameSegments []string
		for _, match := range benchmarkNameRegex.FindAllStringSubmatch(fullTestName, -1) {
			if match[1] != "" {
				nameSegments = append(nameSegments, match[1])
			}
		}
		fullTestName = strings.Join(nameSegments, "/")
	}

	testNameSlash := strings.IndexByte(fullTestName, '/')
	funcName := fullTestName
	if testNameSlash >= 0 {
		funcName = fullTestName[:testNameSlash]
	}
	packageName := getBenchmarkSuiteName(b)

	sourceBounds, _ := ast.GetFuncSourceForName(pc, funcName)
	var testCode string
	if sourceBounds != nil {
		testCode = fmt.Sprintf("%s:%d:%d", sourceBounds.File, sourceBounds.Start.Line, sourceBounds.End.Line)
	}

	var startOptions []opentracing.StartSpanOption
	startOptions = append(startOptions, opentracing.Tags{
		"span.kind":      "test",
		"test.name":      fullTestName,
		"test.suite":     packageName,
		"test.code":      testCode,
		"test.framework": "testing",
		"test.language":  "go",
		"test.type":      "benchmark",
	}, opentracing.StartTime(startTime))

	span, _ := opentracing.StartSpanFromContextWithTracer(context.Background(), instrumentation.Tracer(), fullTestName, startOptions...)
	span.SetBaggageItem("trace.kind", "test")
	avg := math.Round((float64(results.T.Nanoseconds())/float64(results.N))*100) / 100
	span.SetTag("benchmark.runs", results.N)
	span.SetTag("benchmark.duration.mean", avg)
	span.SetTag("benchmark.memory.mean_allocations", results.AllocsPerOp())
	span.SetTag("benchmark.memory.mean_bytes_allocations", results.AllocedBytesPerOp())
	if result {
		span.SetTag("test.status", "PASS")
	} else {
		span.SetTag("test.status", "FAIL")
	}
	span.FinishWithOptions(opentracing.FinishOptions{
		FinishTime: startTime.Add(results.T),
	})
}

func getParentBenchmark(b *testing.B) *testing.B {
	if ptr, err := reflection.GetFieldPointerOfB(b, "parent"); err == nil {
		return *(**testing.B)(ptr)
	}
	return nil
}

func getBenchmarkSuiteName(b *testing.B) string {
	if ptr, err := reflection.GetFieldPointerOfB(b, "importPath"); err == nil {
		return *(*string)(ptr)
	}
	return ""
}

func getBenchmarkHasSub(b *testing.B) int32 {
	if ptr, err := reflection.GetFieldPointerOfB(b, "hasSub"); err == nil {
		return *(*int32)(ptr)
	}
	return 0
}

//Extract benchmark result from the private result field in testing.B
func extractBenchmarkResult(b *testing.B) (*testing.BenchmarkResult, error) {
	if ptr, err := reflection.GetFieldPointerOfB(b, "result"); err == nil {
		return (*testing.BenchmarkResult)(ptr), nil
	} else {
		return nil, err
	}
}

//go:build ignore

// Compiled once at image build time to pre-populate the Go build cache with
// the standard-library packages online-compiler programs commonly import.
package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var _ = []any{
	bufio.NewReader, bytes.NewBuffer, heap.Init, list.New, json.Marshal, errors.New,
	math.Sqrt, big.NewInt, rand.Intn, os.Exit, regexp.MustCompile, slices.Sort[[]int],
	sort.Ints, strconv.Itoa, strings.Split, sync.NewCond, time.Now, unicode.IsDigit,
}

func main() { fmt.Println("warm") }

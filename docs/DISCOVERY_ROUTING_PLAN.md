# Model Discovery-Based Routing Enhancement Plan

## Implementation Status Overview

### Phase 1 (COMPLETED ✅)

**1. Deep Provider Health Validation**
- ✅ Created `internal/discovery/health.go` 
- ✅ Implemented status detection (unreachable/empty/invalid/ok)
- ✅ Integrated with startup and reload processes
- ✅ Health exposed via `/statsz` endpoint
- ✅ Structured logging for provider health

**2. Auto-Generated Agents**
- ✅ Created `internal/discovery/agents.go`
- ✅ Implemented filter functions for all categories
- ✅ Integrated with `gateway.New()`
- ✅ Auto-agents appear in `/v1/models` endpoint
- ✅ User override mechanism working
- ✅ Logging for auto-generation process

### Phase 2 (PRIORITY 🚀)

**1. Registry Dynamic Sync**
Status: ⚠️ NOT IMPLEMENTED

**2. Latency-Based Reordering**
Status: ⚠️ NOT IMPLEMENTED

**3. Advanced Health Checks**
Status: ⚠️ NOT IMPLEMENTED

**4. Context-Aware Filtering**
- ✅ Found implementation at `internal/routing/targets.go:97`
- ✅ Active logic with `autoContextSkip` config
- ✅ Logging for skipped models
- ✅ Working in production

### Phase 3 (PLANNED ⏳)

**1. Model Metadata Enhancement**
Status: ❌ NOT STARTED

**2. Cost Optimization**
Status: ❌ NOT STARTED

## Implementation Roadmap

### Phase 2 Implementation Plan (3-5 days total)

#### Week 1: Registry Dynamic Sync
**Goal**: Keep registry in sync with provider discovery

**Day 1-2: Core infrastructure**
- Create `internal/discovery/sync.go`
  - Cache structures and persistence
  - TTL-based expiration logic
- Add config extension
  - `sync_mode` enum (merge/discovered-only)
  - `sync_ttl_days` and `cache_path`

**Day 3-4: Integration**
- Update discovery process to save results
- Load cached models on startup failure
- Graceful fallback to static registry
- Unit tests for cache operations

**Day 5: Validation**
- Documentation for sync modes
- Test with real provider failures
- Validate TTL expiration behavior

#### Week 2: Latency-Based Reordering
**Goal**: Track and optimize routing performance

**Day 1: Monitoring infrastructure**
- Create `internal/monitoring/latency.go`
  - Thread-safe latency tracking
  - Median calculation functions

**Day 2: Data collection**
- Add latency recording to HTTP client
- Update routing metrics collection
- Add `auto_reorder_by_latency` config

**Day 3: Target sorting**
- Implement sorting in `BuildTargetList`
- Add alternative instances support
- Debug logging for decisions

**Day 4: Testing**
- Benchmark sorting performance
- Test mixed provider scenarios
- Validate latency calculations

#### Week 3: Advanced Health Checks
**Goal**: Detect registry drift and deprecated models

**Day 1: Validation structures**
- Create `internal/discovery/validation.go`
  - Model validation types
  - Comparison functions

**Day 2: Integration**
- Add validation to discovery pipeline
- Build validation report
- Implement severity levels

**Day 3: Warning system**
- Config drift logging
- Startup validation warnings
- Simulated drift testing

**Day 4: Expoures**
- Add validation to `/statsz` endpoint
- Historical trend tracking
- Validation status endpoint

## Risk Assessment

### Prevention Strategies

**1. Zero Breaking Changes**
- All Phase 2 features are opt-in via config flags
- Default behavior unchanged from Phase 1
- Users can ignore new features if needed

**2. Graceful Degradation**
- Discovery failures fall back to static registry
- Cache expiration prevents stale data issues
- Validation errors don't interrupt routing

**3. Comprehensive Testing**
- Unit tests for all new components
- Integration tests with mixed modes
- Manual testing of type systems

**4. Performance Monitoring**
- Benchmark latency tracking overhead
- Measure sorting performance impact
- Validate cache I/O efficiency

## Success Criteria

### Phase 2 Completion Checklist

✅ All 3 Phase 2 features implemented
✅ Unit tests passing (minimum 90% coverage)
✅ Integration tests successful
✅ Documentation updated
✅ Feature flags working correctly
✅ No breaking changes confirmed

### Quality Gates

- **Code Coverage**: Minimum 90% for new code
- **Performance**: < 5% increase in routing latency
- **Memory**: < 2MB additional heap usage
- **Testing**: At least 3 integration test scenarios
- **Documentation**: All new config options documented

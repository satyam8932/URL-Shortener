-- Token bucket for one client, run atomically inside Redis.
-- KEYS[1]: bucket key. ARGV[1]: refill rate in tokens per second. ARGV[2]: burst.
-- Returns {allowed (0 or 1), milliseconds until the next token}.

local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])

-- Redis's clock, not the caller's, so instances with skewed clocks agree.
local time = redis.call('TIME')
local now = tonumber(time[1]) + tonumber(time[2]) / 1000000

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'updated')
local tokens = tonumber(bucket[1]) or burst
local updated = tonumber(bucket[2]) or now

tokens = math.min(burst, tokens + math.max(0, now - updated) * rate)

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_after_ms = math.ceil((1 - tokens) / rate * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'updated', tostring(now))
-- A bucket idle this long is full again, which is the same as having no
-- bucket, so Redis can drop it.
redis.call('EXPIRE', KEYS[1], math.ceil(burst / rate))

return {allowed, retry_after_ms}

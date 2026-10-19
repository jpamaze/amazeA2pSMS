-- KEYS[1] wallet hash (balance, credit_limit, held)
-- KEYS[2] holds hash (hold_id -> amount)
-- ARGV[1] hold_id, ARGV[2] amount
-- Returns 1 when held (or already held), 0 when insufficient.
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 1 then
  return 1
end
local amount = tonumber(ARGV[2])
local w = redis.call('HMGET', KEYS[1], 'balance', 'credit_limit', 'held')
local balance = tonumber(w[1] or '0')
local limit = tonumber(w[2] or '0')
local held = tonumber(w[3] or '0')
if balance + limit - held < amount then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'held', amount)
redis.call('HSET', KEYS[2], ARGV[1], amount)
return 1

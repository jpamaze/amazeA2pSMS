-- KEYS[1] wallet hash
-- ARGV[1] amount, ARGV[2] database balance, ARGV[3] database credit limit
-- Loads a wallet missing from Redis from the database values, then debits
-- amount if balance + credit_limit - held covers it. Returns the new balance,
-- or nil when it does not.
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'balance', ARGV[2], 'credit_limit', ARGV[3], 'held', 0)
end
local w = redis.call('HMGET', KEYS[1], 'balance', 'credit_limit', 'held')
local available = tonumber(w[1] or '0') + tonumber(w[2] or '0') - tonumber(w[3] or '0')
if available < tonumber(ARGV[1]) then
  return false
end
return redis.call('HINCRBY', KEYS[1], 'balance', -tonumber(ARGV[1]))

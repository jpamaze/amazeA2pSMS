-- KEYS[1] wallet hash
-- ARGV[1] new credit limit, ARGV[2] database balance
-- Loads a wallet missing from Redis from the database balance; otherwise only
-- the limit changes (the Redis balance runs ahead of the database between
-- settlement batches and must not be overwritten).
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'balance', ARGV[2], 'credit_limit', ARGV[1], 'held', 0)
else
  redis.call('HSET', KEYS[1], 'credit_limit', ARGV[1])
end
return 1

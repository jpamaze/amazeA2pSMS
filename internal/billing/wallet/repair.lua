-- KEYS[1] wallet hash, KEYS[2] holds hash
-- ARGV[1] balance, ARGV[2] credit_limit, ARGV[3..] hold IDs to drop
-- Sets balance and limit from the system of record, drops the given holds
-- and recomputes held from the holds that remain, in one step so holds
-- placed meanwhile are counted. Returns {old_held, new_held, dropped}.
local old = tonumber(redis.call('HGET', KEYS[1], 'held') or '0')
local dropped = 0
for i = 3, #ARGV do
  dropped = dropped + redis.call('HDEL', KEYS[2], ARGV[i])
end
local held = 0
for _, v in ipairs(redis.call('HVALS', KEYS[2])) do
  held = held + tonumber(v)
end
redis.call('HSET', KEYS[1], 'balance', ARGV[1], 'credit_limit', ARGV[2], 'held', held)
return {old, held, dropped}

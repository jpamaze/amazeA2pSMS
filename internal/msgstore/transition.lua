-- KEYS[1] msg hash
-- ARGV[1] status, ARGV[2] rank, ARGV[3] error code, ARGV[4] time ms, ARGV[5] final (1/0)
-- Returns 1 changed, 0 not forward progress, -1 no record.
if redis.call('EXISTS', KEYS[1]) == 0 then
  return -1
end
local cur = tonumber(redis.call('HGET', KEYS[1], 'rank') or '0')
if tonumber(ARGV[2]) <= cur then
  return 0
end
redis.call('HSET', KEYS[1], 'status', ARGV[1], 'rank', ARGV[2])
if ARGV[3] ~= '' then
  redis.call('HSET', KEYS[1], 'err', ARGV[3])
end
if ARGV[5] == '1' then
  redis.call('HSET', KEYS[1], 'done', ARGV[4])
end
return 1

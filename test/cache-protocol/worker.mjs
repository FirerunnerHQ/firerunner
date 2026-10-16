import * as cache from '@actions/cache';
const [operation, key, restoreKeys] = process.argv.slice(2);
const value = operation === 'save'
  ? await cache.saveCache(['payload'], key)
  : await cache.restoreCache(['payload'], key, JSON.parse(restoreKeys));
console.log(`RESULT:${JSON.stringify({value: value ?? null})}`);

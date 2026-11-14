// Runs a solc-js build (soljson-v….js) as a command-line compiler, for the
// node (compiler/toolchain.go): node runner.cjs SOLJSON --version, or
// node runner.cjs SOLJSON --standard-json < input.json.
const soljson = require(process.argv[2]);

if (process.argv[3] === '--version') {
  process.stdout.write(`${soljson.cwrap('solidity_version', 'string', [])()}\n`);
} else {
  const compile = soljson.cwrap('solidity_compile', 'string', ['string', 'number', 'number']);
  const chunks = [];
  process.stdin.on('data', (chunk) => chunks.push(chunk));
  process.stdin.on('end', () => process.stdout.write(compile(Buffer.concat(chunks).toString('utf8'), 0, 0)));
}

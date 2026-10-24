// Runs inside TestPhotoDecoderSafety, without browser or network dependencies.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.join(__dirname, '../internal/handler/static/photo-codecs/v3');
function environment() {
  const ctx = vm.createContext({console, ArrayBuffer, Uint8Array, Uint8ClampedArray, setTimeout});
  ctx.self = ctx;
  ctx.importScripts = (...names) => names.forEach(name => vm.runInContext(fs.readFileSync(path.join(root, name), 'utf8'), ctx));
  ctx.importScripts('pako.min.js', 'UTIF.js');
  return ctx;
}
(async () => {
  const ctx = environment(), utif = ctx.UTIF;
  utif.ttypes[256] = 4;
  const cycle = Uint8Array.from(Buffer.from('49492a0008000000000008000000', 'hex')).buffer;
  assert.throws(() => utif.decode(cycle), /IFD/);
  const chain = new ArrayBuffer(8 + 129 * 6), view = new DataView(chain);
  view.setUint16(0, 0x4949); view.setUint16(2, 42, true); view.setUint32(4, 8, true);
  for (let i = 0; i < 128; i++) view.setUint32(10 + i*6, 14 + i*6, true);
  assert.throws(() => utif.decode(chain), /IFD/);
  const recursive = utif.encode([{t34665: [8]}]);
  assert.throws(() => utif.decode(recursive), /IFD/);
  const malformed = utif.encode([{t256: [1]}]);
  new DataView(malformed).setUint32(14, 0xffffffff, false);
  assert.throws(() => utif.decode(malformed), /TIFF/);

  const fixture = fs.readFileSync(path.join(__dirname, '../internal/handler/testdata/photos/deflate-overflow.tiff'));
  const overflow = fixture.buffer.slice(fixture.byteOffset, fixture.byteOffset + fixture.byteLength);
  const page = utif.decode(overflow)[0];
  let chunks = 0, largest = 0;
  const Inflate = ctx.pako.Inflate;
  ctx.pako.Inflate = function(options) {
    const stream = new Inflate(options);
    const push = stream.push;
    stream.push = function(...args) {
      const onData = this.onData;
      this.onData = chunk => { chunks++; largest = Math.max(largest, chunk.length); return onData(chunk); };
      return push.apply(this, args);
    };
    return stream;
  };
  assert.throws(() => utif.decodeImage(overflow, page), /Deflate output too large/);
  assert.equal(chunks, 1); assert.equal(largest, 16384);
  // A strip must not borrow space from the next strip in the image buffer.
  const compressed = require('node:zlib').deflateSync(Buffer.alloc(2));
  const image = {width:1,height:2,t258:[8],t277:[1]};
  assert.throws(() => utif.decode._decompress(image, [], compressed, 0, compressed.length, 8, new Uint8Array(2), 0, 1, 1), /Deflate output too large/);
  const target = new Uint8Array(2);
  utif.decode._decompress(image, [], compressed, 0, compressed.length, 8, target, 0, 1, 2);
  // Exercise the actual adapter: size checks must precede loading codecs and pixels.
  vm.runInContext(fs.readFileSync(path.join(root, 'decoder.js'), 'utf8'), ctx);
  let result, loaded = false, displayed = false;
  ctx.postMessage = value => { result = value; };
  ctx.importScripts = () => { loaded = true; };
  await ctx.onmessage({data: {buffer: new ArrayBuffer(80000001), kind: 'heif'}});
  assert.equal(result.error, 'Could not read this photo'); assert.equal(loaded, false);
  ctx.libheif = async () => ({HeifDecoder: class {
    decode() { return [{is_primary: () => true, get_width: () => 50000001, get_height: () => 1, display: () => { displayed = true; }}]; }
  }});
  await ctx.onmessage({data: {buffer: new ArrayBuffer(1), kind: 'heif'}});
  assert.equal(result.error, 'Could not read this photo'); assert.equal(displayed, false);
  await ctx.onmessage({data: {buffer: utif.encode([{t256:[50000001], t257:[1]}]), kind:'tiff'}});
  assert.equal(result.error, 'Could not read this photo');
  await ctx.onmessage({data: {buffer: cycle, kind:'tiff'}});
  assert.equal(result.error, 'Could not read this photo');
  const small = utif.encodeImage(new Uint8Array([10,20,30,255]), 1, 1);
  await ctx.onmessage({data: {buffer: small, kind:'tiff'}});
  assert.equal(result.width, 1); assert.equal(result.height, 1);
  assert.deepEqual(Array.from(new Uint8Array(result.buffer)), [10,20,30,255]);
  console.log('PASS TIFF cycle, recursive/count/value limits, worker byte/pixel caps, and valid TIFF');
})().catch(error => { console.error(error); process.exitCode = 1; });

const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/handler/static/photo-process.js','utf8');
const tick = () => new Promise(resolve => setImmediate(resolve));
function png(width=1,height=1) {
 const b=Buffer.alloc(24);Buffer.from('89504e470d0a1a0a','hex').copy(b);b.write('IHDR',12);b.writeUInt32BE(width,16);b.writeUInt32BE(height,20);return b;
}
function file(bytes,name='photo.png') { const b=new Blob([bytes]);b.name=name;return b; }
function harness(extra={}) {
 const timers=new Map();let next=0;
 const ctx=vm.createContext({Blob,Uint8Array,DataView,ArrayBuffer,
  setTimeout(fn,ms){const id=++next;timers.set(id,{fn,ms});return id;},clearTimeout(id){timers.delete(id);},...extra});
 vm.runInContext(source,ctx);return {photos:ctx.SHPhotos,timers};
}
test('native header pixel limits run before any browser decode',async()=>{
 let calls=0;const {photos}=harness({createImageBitmap(){calls++;throw Error('should not decode');}});
 const jpeg=Buffer.from('ffd8ffc0000b08fffffffF03011100ffd9','hex');
 const bmp=Buffer.alloc(54);bmp.write('BM');bmp.writeUInt32LE(40,14);bmp.writeInt32LE(50000001,18);bmp.writeInt32LE(1,22);
 const webp=Buffer.alloc(30);webp.write('RIFF');webp.write('WEBP',8);webp.write('VP8X',12);webp.writeUInt32LE(10,16);webp.fill(255,24,30);
 const gif=Buffer.alloc(14);gif.write('GIF89a');gif.writeUInt16LE(65535,6);gif.writeUInt16LE(65535,8);gif[13]=59;
 const box=(type,data)=>{const b=Buffer.alloc(data.length+8);b.writeUInt32BE(b.length);b.write(type,4);data.copy(b,8);return b;};
 const ispe=Buffer.alloc(12);ispe.writeUInt32BE(50000001,4);ispe.writeUInt32BE(1,8);
 const avif=Buffer.concat([box('ftyp',Buffer.from('avif0000')),box('meta',Buffer.concat([Buffer.alloc(4),box('iprp',box('ipco',box('ispe',ispe)))]))]);
 for(const bytes of [png(50000001),jpeg,bmp,webp,gif,avif,Buffer.from('unsupported')]) await assert.rejects(photos.shrink(file(bytes)),/Could not read/);
 assert.equal(calls,0);
});
test('a stalled native bitmap hits the deadline and its late result is closed',async()=>{
 let resolve,closed=0;const {photos,timers}=harness({createImageBitmap:()=>new Promise(r=>resolve=r)});
 const result=photos.shrink(file(png()));const rejection=assert.rejects(result,/Could not read/);
 await tick();assert.equal(typeof resolve,'function');[...timers.values()][0].fn();await rejection;
 resolve({width:1,height:1,close(){closed++;}});await tick();assert.equal(closed,1);assert.equal(timers.size,0);
});
test('image fallback times out, clears handlers, and releases its source',async()=>{
 let img;
 const {photos,timers}=harness({FileReader:class {readAsDataURL(){this.result='data:image/png;base64,AA==';this.onload();}},Image:class {constructor(){img=this;}}});
 const result=photos.shrink(file(png()));const rejection=assert.rejects(result,/Could not read/);await tick();assert.ok(img.src);
 [...timers.values()][0].fn();await rejection;assert.equal(img.src,'');assert.equal(img.onload,null);assert.equal(img.onerror,null);
});
test('a header read has a deadline without starting a decoder',async()=>{
 const {photos,timers}=harness();const result=photos.shrink({size:5,slice:()=>({arrayBuffer:()=>new Promise(()=>{})})});
 const rejection=assert.rejects(result,/Could not read/);[...timers.values()][0].fn();await rejection;
});
test('a late codec file read cannot create a worker after timeout',async()=>{
 let resolve,workers=0;const head=file(Buffer.from('II*\0'),'scan.tif');head.arrayBuffer=()=>new Promise(r=>resolve=r);
 const {photos,timers}=harness({Worker:class{constructor(){workers++;}}});const result=photos.shrink(head);const rejection=assert.rejects(result,/Could not read/);
 await tick();[...timers.values()][0].fn();await rejection;resolve(new ArrayBuffer(4));await tick();assert.equal(workers,0);
});
test('a native result exceeding the ceiling is closed without another decode',async()=>{
 let closed=0,fallbacks=0;
 const {photos}=harness({createImageBitmap:async()=>({width:50000001,height:1,close(){closed++;}}),FileReader:class {constructor(){fallbacks++;}}});
 await assert.rejects(photos.shrink(file(png())),/Could not read/);assert.equal(closed,1);assert.equal(fallbacks,0);
});

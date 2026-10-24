const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
async function page({commit=true,conflict=false}={}) {
 const nodes=new Map(),writes=[],reads=[];let files=[{path:'images/front.webp',size:3}],version=1;
 function node(){return {children:[],events:{},value:'',textContent:'',hidden:false,dataset:{},classList:{add(){},remove(){}},
  appendChild(el){this.children.push(el);},setAttribute(){},addEventListener(k,fn){this.events[k]=fn;},
  replaceChildren(){this.children=[];},querySelectorAll(){return [];},get options(){return this.children;}};}
 const get=id=>{if(!nodes.has(id))nodes.set(id,node());return nodes.get(id);};
 const ctx=vm.createContext({document:{getElementById:get,createElement:node},window:{addEventListener(){}},location:{search:'?site=alice/demo'},history:{replaceState(){}},
  URLSearchParams,URL:{createObjectURL:()=> 'blob:preview',revokeObjectURL(){}},setTimeout,clearTimeout,crypto:require('node:crypto').webcrypto,
  FormData:class{constructor(){this.files=[];}append(_,blob,name){this.files.push({blob,name});}},
  fetch:async(path,options)=>{
   if(options.method==='POST') {
    writes.push(options);
    if(commit||writes.length>1){files.push(...options.body.files.map(f=>({path:'images/'+f.name,size:3,sha256:conflict?'different':'abc'})));version++;}
    if(writes.length===1)throw Error('Connection lost');
    return {ok:true,json:async()=>({site:'alice/demo',version,photos:options.body.files.map(f=>({path:'images/'+f.name,url:'https://site.test/images/'+f.name}))})};
   }
   reads.push(path);
   const body=path==='/api/me'?{username:'alice'}:path==='/api/collaboration/sites'?[{name:'demo',owner_username:'alice',access_role:'owner'}]:path.includes('/files?')?{files}:{active_version:version,etag:'v'+version,url:'https://site.test/'};
   return {ok:true,json:async()=>body};
  }});
 vm.runInContext(fs.readFileSync('internal/handler/static/photo-process.js','utf8'),ctx);
 ctx.SHPhotos.shrink=async()=>({blob:{size:3},ext:'.webp'});ctx.SHPhotos.digest=async()=> 'abc';
 vm.runInContext(fs.readFileSync('internal/handler/static/photos.js','utf8'),ctx);
 await new Promise(resolve=>setImmediate(resolve));
 await get('files').events.change({target:{files:[{name:'front.jpg',size:100}]}});
 return {get,writes,reads,save:()=>get('save').events.click()};
}
test('/photos reconciles a lost committed reply using the reserved collision name and hash',async()=>{
 const m=await page();await m.save();assert.equal(m.writes.length,1);assert.equal(m.writes[0].body.files[0].name,'front-2.webp');
 assert.equal(m.writes[0].headers['If-Match'],'v1');assert.match(m.get('status').textContent,/uncertain/);
 await m.save();assert.equal(m.writes.length,1);assert.equal(m.get('done').hidden,false);assert.match(m.get('summary').textContent,/images\/front-2.webp/);
 assert.ok(m.reads.at(-1).includes('sha256=images%2Ffront-2.webp'));
});
test('/photos retries an uncommitted batch with the same names and current ETag',async()=>{
 const m=await page({commit:false});await m.save();await m.save();assert.equal(m.writes.length,2);
 assert.equal(m.writes[0].body.files[0].name,m.writes[1].body.files[0].name);assert.equal(m.get('done').hidden,false);
});
test('/photos refuses a conflicting file after an uncertain save',async()=>{
 const m=await page({conflict:true});await m.save();await m.save();assert.equal(m.writes.length,1);assert.match(m.get('status').textContent,/file changed/);
});

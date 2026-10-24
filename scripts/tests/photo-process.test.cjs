const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const context = vm.createContext({ TextEncoder });
vm.runInContext(fs.readFileSync('internal/handler/static/photo-process.js', 'utf8'), context);
vm.runInContext(fs.readFileSync('internal/mcp/apps/photo-picker.js', 'utf8'), context);
const {slug, shrink} = context.SHPhotos;
const {occupiedPaths, photoPath, batches} = context.SHPhotoBatch;
test('readable names match /photos', () => {
  assert.equal(slug('Front door.HEIC'), 'front-door');
  assert.equal(slug('...☀.jpg'), 'photo');
  assert.equal(slug('x'.repeat(100) + '.png').length, 60);
});
test('collisions, suffix truncation, and directory conflicts match /photos', () => {
  const occupied = occupiedPaths([{path:'index.html'}, {path:'images/front.webp'}, {path:'images/front-2.webp'}]);
  assert.equal(photoPath(occupied, 'front.webp'), 'images/front-3.webp');
  assert.equal(photoPath(occupied, 'front.webp'), 'images/front-4.webp');
  const name = 'x'.repeat(60) + '.jpg';
  assert.equal(photoPath(occupied, name), 'images/' + name);
  assert.equal(photoPath(occupied, name), 'images/' + 'x'.repeat(58) + '-2.jpg');
  assert.throws(() => photoPath(occupiedPaths([{path:'images'}]), 'front.jpg'), /images path is a file/);
  assert.throws(() => photoPath(occupiedPaths([{path:'images/front.jpg/child'}]), 'front.jpg'), /folder uses/);
});
test('one selection creates one version or refuses before writing', () => {
 const photos=Array.from({length:30},(_,i)=>({path:'images/photo-'+i+'.webp',base64:'a'.repeat(300000)}));
 assert.equal(batches(photos,'alice/demo',16*1024*1024).length,1);
 assert.throws(()=>batches(photos,'alice/demo',810000),/fewer photos/);
});
test('reject oversized originals before decoding', async () => {
  await assert.rejects(shrink({size:80000001}), /Could not read this photo/);
});
test('bridge ignores foreign sources and malformed messages and handles the standard handshake', async () => {
  const sent = [], origins = [], listeners = {}, parent = {postMessage: (message, origin) => {sent.push(message); origins.push(origin);}};
  const root = {dataset:{},style:{setProperty(){}}};
  const bridge = vm.createContext({
    window:{parent,addEventListener:(type,fn)=>{listeners[type]=fn;}},
    document:{documentElement:root,body:{getBoundingClientRect:()=>({height:120})}},
    matchMedia:()=>({matches:false}), ResizeObserver:class{observe(){}}, URL, setTimeout, clearTimeout
  });
  vm.runInContext(fs.readFileSync('internal/mcp/apps/bridge.js','utf8'),bridge);
  const app=bridge.window.SHApp, start=app.start();
  assert.equal(sent[0].method,'ui/initialize');assert.equal(origins[0],'*');
  const reply={jsonrpc:'2.0',id:sent[0].id,result:{hostInfo:{name:'Test host'},hostContext:{theme:'dark'}}};
  listeners.message({source:{},data:reply});
  assert.equal(root.dataset.theme,'light');
  listeners.message({source:parent,origin:'https://chat.example',data:{...reply,jsonrpc:'1.0'}});
  assert.equal(root.dataset.theme,'light');
  listeners.message({source:parent,origin:'https://chat.example',data:reply});await start;assert.equal(origins[1],'https://chat.example');
  assert.equal(root.dataset.theme,'dark');
  assert.equal(sent[1].method,'ui/notifications/initialized');
  assert.equal(sent[2].method,'ui/notifications/size-changed');
  let input;
  app.on('tool-input',p=>input=p.arguments);
  listeners.message({source:parent,origin:'https://chat.example',data:{jsonrpc:'2.0',method:'ui/notifications/tool-input',params:{arguments:{site:'test'}}}});
  assert.equal(input.site,'test');
  const call=app.callTool('list_sites');
  const request=sent.at(-1);
  assert.equal(request.method,'tools/call');assert.equal(origins.at(-1),'https://chat.example');
  listeners.message({source:parent,origin:'https://chat.example',data:{jsonrpc:'2.0',id:request.id,result:{structuredContent:{sites:[]}}}});
  assert.equal((await call).sites.length,0);
  await assert.rejects(app.openLink('http://example.com'),/HTTPS/);
});

function picker({commit = true, fail = true, failAt = 1, maxBodyBytes = 16000000} = {}) {
  const nodes = new Map(), handlers = {}, writes = []; let files = [], failed = false;
  const node = () => ({hidden:false,children:[],events:{},textContent:'',
    appendChild(el){this.children.push(el);}, setAttribute(k,v){this[k]=v;},removeAttribute(k){delete this[k];},
    addEventListener(k,fn){this.events[k]=fn;},remove(){this.removed=true;},replaceChildren(){this.children=[];}});
  const get = id => {if(!nodes.has(id))nodes.set(id,node());return nodes.get(id);};
  const document = {getElementById:get,createElement:node,body:{hidden:false}};
  const app = {hostCapabilities:{serverTools:{}},on:(k,fn)=>handlers[k]=fn,start:async()=>{},callTool:async(name,args)=>{
    if(name==='get_site')return {etag:'v1',active_version:1};
    if(name==='list_site_files')return {files};
    if(name==='read_site_file')return {encoding:'base64',content:files.find(f=>f.path===args.path)?.size===3?'YWJj':'different'};
    if(name==='deploy_site'){
      writes.push(args);if(commit || failed) files=args.files.map(file=>({path:file.path,size:3}));
      if(fail && writes.length === failAt && !failed){failed=true;throw Error('The chat app did not respond. Try again.');}
      return {};
    }
  }};
  const ctx = vm.createContext({document,window:{addEventListener(){}},SHApp:app,SHPhotoConfig:{maxBodyBytes},
    SHPhotos:{slug, dataURL:async()=> 'data:image/webp;base64,YWJj',shrink:async f=>{if(f.bad)throw Error('bad');return {blob:{size:3},ext:'.webp'};}}, TextEncoder});
  vm.runInContext(fs.readFileSync('internal/mcp/apps/photo-picker.js','utf8'),ctx);
  handlers['tool-input']({arguments:{site:'blog'}});handlers['tool-result']({structuredContent:{site:'alice/blog'}});
  return {get,document,handlers,writes,setFiles:value=>files=value,
    add:files=>get('files').events.change({target:{files,value:''}}),save:()=>get('finish').events.click()};
}
test('lost save reply reconciles the reserved path and byte size without a second version', async()=>{
 const m=picker();await m.add([{name:'front.jpg'}]);await m.save();assert.equal(m.writes.length,1);
 await m.save();assert.equal(m.writes.length,1);assert.equal(m.get('done').textContent,'Added 1 photo to images/');
});
test('unconfirmed save retries the reserved path, including a mismatched size', async()=>{
 for(const files of [[]]){
  const m=picker({commit:false});await m.add([{name:'front.jpg'}]);await m.save();m.setFiles(files);await m.save();
  assert.equal(m.writes.length,2);assert.deepEqual(m.writes[0].files.map(f=>f.path),m.writes[1].files.map(f=>f.path));
  assert.equal(m.get('done').textContent,'Added 1 photo to images/');
 }
});
test('failed selections release capacity; unsaved photos can be removed and only saved photos count', async()=>{
 const m=picker({fail:false});await m.add(Array.from({length:30},()=>({name:'bad.heic',bad:true})));
 assert.equal(m.get('add').hidden,false);assert.equal(m.get('finish').disabled,true);
 await m.add([{name:'front.jpg'},{name:'back.jpg'}]);assert.equal(m.get('add').hidden,false);
 const row=m.get('photos').children.at(-1);await row.children.at(-1).events.click();assert.equal(row.removed,true);
 await m.save();assert.equal(m.get('done').textContent,'Added 1 photo to images/');assert.equal(m.writes[0].files.map(f=>f.path).length,1);
});
test('archive input hides the entire frame before any result and stays hidden',()=>{
 const m=picker();m.handlers['tool-input']({arguments:{site:'blog',archive:true}});assert.equal(m.document.body.hidden,true);
 m.handlers['tool-result']({structuredContent:{upload_url:'https://example.test/upload'}});
 assert.equal(m.document.body.hidden,true);
});

test('conflicting bytes after an uncertain save are never overwritten',async()=>{
 const m=picker({commit:false});await m.add([{name:'front.jpg'}]);await m.save();
 m.setFiles([{path:'images/front.webp',size:9}]);await m.save();
 assert.equal(m.writes.length,1);assert.match(m.get('status').textContent,/file changed/);
});
test('writes preserve existing files and carry the site ETag and team namespace',async()=>{
 const m=picker({fail:false});m.handlers['tool-result']({structuredContent:{site:'team-sales/blog'}});
 await m.add([{name:'front.jpg'}]);await m.save();
 assert.equal(m.writes[0].owner,'team-sales');assert.equal(m.writes[0].site,'blog');assert.equal(m.writes[0].etag,'v1');
 assert.equal(m.writes[0].keep_other_files,true);assert.equal(m.writes[0].intent,'update');
});
async function photosPage({account='alice',handle='bob',sites=[{name:'blog',owner_username:handle,access_role:'owner'}],signedIn=true}={}) {
 const elements=new Map(),session=new Map();let signIn;
 const element=()=>({children:[],events:{},value:'',textContent:'',hidden:false,
  appendChild(el){this.children.push(el);},replaceChildren(){this.children=[];this.value='';},
  addEventListener(k,fn){this.events[k]=fn;},querySelectorAll(){return [];},
  get options(){return this.children;},set selectedIndex(value){if(value===-1)this.value='';}});
 const get=id=>{if(!elements.has(id))elements.set(id,element());return elements.get(id);};
 const search='?site=blog'+(account?'&account='+account:'');
 const ctx=vm.createContext({document:{getElementById:get,createElement:element},
  window:{addEventListener(){},SHSignIn:{mount:(_,options)=>{signIn=options;return {};}}},
  location:{search,hash:'',origin:'https://example.test',assign(){}},history:{},
  fetch:async path=>({ok:true,json:async()=>path==='/api/me'?{username:handle}:path==='/api/collaboration/sites'?sites:{}}),
  crypto:require('node:crypto').webcrypto,btoa,TextEncoder,Uint8Array,URLSearchParams,SHPhotos:{slug}});
 vm.runInContext(fs.readFileSync('internal/handler/static/photos.js','utf8'),ctx);
 await new Promise(resolve=>setImmediate(resolve));
 return {get,session,signIn,search};
}
test('photo sign-in link names a different account without selecting its same-named site',async()=>{
 for(const sites of [[{name:'blog',owner_username:'bob',access_role:'owner'}],[]]){
  const m=await photosPage({sites});
  assert.equal(m.get('site').value,'');assert.equal(m.get('save').disabled,true);
  assert.match(m.get('status').textContent,sites.length?/This link is for alice; you are signed in as bob/:/no sites/);
 }
 for(const options of [{handle:'alice'},{account:''}]){
  const m=await photosPage(options);assert.equal(m.get('site').value,(options.handle||'bob')+'/blog');assert.equal(m.get('status').textContent,'');
 }
});

test('bridge pins the parent origin for replies and notifications',async()=>{
 const listeners={},sent=[],parent={postMessage:message=>sent.push(message)};
 const root={dataset:{},style:{setProperty(){}}};
 const ctx=vm.createContext({window:{parent,addEventListener:(name,fn)=>listeners[name]=fn},document:{documentElement:root,body:{getBoundingClientRect:()=>({height:1})}},matchMedia:()=>({matches:false}),ResizeObserver:class{observe(){}},URL,setTimeout,clearTimeout});
 vm.runInContext(fs.readFileSync('internal/mcp/apps/bridge.js','utf8'),ctx);
 const app=ctx.window.SHApp,ready=app.start();
 listeners.message({source:parent,origin:'https://host.example',data:{jsonrpc:'2.0',id:sent[0].id,result:{}}});await ready;
 let count=0;app.on('tool-result',()=>count++);
 for(const origin of ['https://other.example','null','https://host.example'])listeners.message({source:parent,origin,data:{jsonrpc:'2.0',method:'ui/notifications/tool-result',params:{}}});
 assert.equal(count,1);
});

const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const context = vm.createContext({URL});
vm.runInContext(fs.readFileSync('internal/mcp/apps/my-sites.js', 'utf8'), context);
const {renderEntry, node, badge} = context.SHSites;
// A DOM double rejects HTML parsing; tests exercise the production constructors.
class Element {
  constructor(tag) { this.tag = tag; this.children = []; }
  set innerHTML(_) { throw Error('HTML parsing is forbidden'); }
  set textContent(value) { this.text = value; }
  append(...children) { this.children.push(...children); }
}
const document = {createElement: tag => new Element(tag)};
test('hostile site names and visitor entries are text, never markup', () => {
  for (const hostile of ['<img src=x onerror=alert(1)>', '\"><script>alert(1)</script>']) {
    const site = node(document, 'strong', '', hostile);
    assert.equal(site.text, hostile);
    assert.equal(site.children.length, 0);
    const entry = renderEntry(document, {name: hostile, message: hostile, created_at: '2026-10-23'});
    assert.equal(entry.children[0].text, hostile);
    assert.equal(entry.children[1].text, hostile);
    assert.equal(entry.children[1].children.length, 0);
  }
});
test('Enterprise access badges describe company and team access',()=>{
 assert.equal(badge({access:'only_me'}),'Owner or team');assert.equal(badge({access:'network'}),'Network');assert.equal(badge({access:'company'}),'Company');
});

function mountSites(callTool=async()=>({})) {
 const elements=new Map(),handlers={},queries=[];
 const element=()=>({children:[],dataset:{},style:{},value:'',hidden:false,textContent:'',events:{},
  append(...nodes){this.children.push(...nodes);},replaceChildren(){this.children=[];this.textContent='';},setAttribute(k,v){this[k]=v;},
  addEventListener(k,fn){this.events[k]=fn;},focus(){},remove(){}});
 const get=id=>{if(!elements.has(id))elements.set(id,element());return elements.get(id);};
 const app={hostCapabilities:{serverTools:{}},on:(k,fn)=>handlers[k]=fn,start:async()=>{},
  callTool:async(name,args)=>{queries.push({name,...args});return callTool(name,args);},callToolResult:async()=>{throw Error('unavailable');}};
 const ctx=vm.createContext({document:{getElementById:get,createElement:tag=>{assert.notEqual(tag,'iframe');return element();},addEventListener(){},body:{dataset:{dashboard:'true'}}},SHApp:app,URL,IntersectionObserver:class {constructor(){throw Error('My sites must not observe thumbnails');}}});
 vm.runInContext(fs.readFileSync('internal/mcp/apps/my-sites.js','utf8'),ctx);
 return {get,handlers,queries};
}
test('every site uses its initial tile without nested frames or preview URLs',()=>{
 const m=mountSites();
 const sites=[
  {name:'public',summary:{title:'Harbor'},display:{access:'anyone',passcode_protected:false}},
  {name:'private',display:{access:'specific',passcode_protected:false}},
  {name:'locked',display:{access:'anyone',passcode_protected:true}},
  {name:'offline',offline:true}, {name:'unknown'}, {name:''}
 ].map(site=>({...site,url:'https://shop.alice.example.test/'}));
 m.handlers['tool-result']({structuredContent:{items:sites}});
 assert.deepEqual(m.get('rows').children.map(row=>row.children[0].textContent),['H','P','L','O','U','?']);
 for(const row of m.get('rows').children){
  assert.equal(row.children[0].children.length,0);
  assert.equal(row.children[0].dataset.url,undefined);
 }
 m.get('search').value='harbor';m.get('search').events.input();
 assert.equal(m.get('rows').children.length,1);
 assert.equal(m.get('rows').children[0].children[0].textContent,'H');
});
test('initial list errors and cancellation end loading',()=>{
 const m=mountSites();
 m.handlers['tool-result']({isError:true});assert.match(m.get('list-status').textContent,/Could not load/);assert.equal(m.get('rows').children.length,0);
 m.handlers['tool-cancelled']({});assert.match(m.get('list-status').textContent,/cancelled/);
});
test('filtered inputs label local search scope',()=>{
 const m=mountSites();
 for(const key of ['include']){
  m.handlers['tool-input']({arguments:{[key]:1}});
  m.handlers['tool-result']({structuredContent:{items:[{name:'blog'}]}});
  assert.equal(m.get('view-label').textContent,'Showing 1 sites');
 }
});
test('later tool errors and cancellations preserve the loaded list and dashboard back navigation',()=>{
 for(const sites of [[],[{name:'blog'}]]){
  const m=mountSites();m.handlers['tool-result']({structuredContent:{items:sites}});
  if(sites.length)m.get('rows').children[0].events.click();
  for(const notify of [()=>m.handlers['tool-result']({isError:true}),()=>m.handlers['tool-cancelled']({})]){
   notify();assert.equal(m.get('list-status').textContent,sites.length?'':'No sites yet.');
  }
  if(sites.length)m.get('back').events.click();
  assert.equal(m.get('rows').children.length,sites.length);
  assert.equal(m.get('list-status').textContent,sites.length?'':'No sites yet.');
 }
});
test('dashboard reads Postgres tables through existing tools and isolates failures',async()=>{
 const m=mountSites(async(name,args)=>{
  if(name==='site_activity')return {visits:{unique_viewers:2,from:'2026-10-18T00:00:00Z',days:[{day:'2026-10-19',unique_viewers:2}]}};
  if(name==='storage_list_resources')return {resources:[
   {kind:'kv',name:'settings'}, {kind:'files',name:'uploads'},
   {kind:'db',name:'forms',tables:{private:{preset:'private'},broken:{preset:'inbox'},leads:{preset:'records'}}}
  ]};
  assert.equal(name,'storage_list_rows');assert.equal(args.resource,'forms');assert.equal(args.name,undefined);assert.equal(args.owner,'team-sales');assert.equal(args.order,'created_at');assert.equal(args.desc,true);assert.equal(args.limit,3);
  if(args.table==='broken')throw Error('unavailable');
  assert.equal(args.table,'leads');return {rows:[{id:3,name:'<img onerror=alert(1)>',created_at:'2026-10-24'}, {id:2,name:'Second',created_at:'2026-10-23'}]};
 });
 m.handlers['tool-result']({structuredContent:{items:[{name:'blog',owner_username:'team-sales'}]}});m.get('rows').children[0].events.click();
 await new Promise(resolve=>setImmediate(resolve));
 assert.equal(m.get('visitor-count').textContent,'2');assert.equal(m.get('chart').children.length,7);
 assert.equal(m.get('entries').hidden,false);assert.equal(m.get('entry-rows').children.length,2);assert.match(m.get('entries-error').textContent,/broken/);
 assert.equal(m.get('entry-rows').children[0].children[0].textContent,'<img onerror=alert(1)>');
});

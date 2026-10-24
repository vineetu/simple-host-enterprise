package handler

const notificationsPanel = `
<style>
#notifications-section label { display:block; margin:10px 0; }
#notifications-section input:not([type=checkbox]), #notifications-section select { max-width:100%; box-sizing:border-box; }
#notifications-section pre, #notifications-section p { white-space:pre-wrap; overflow-wrap:anywhere; }
#notifications-section button { margin:4px; }
</style>
<section id="notifications-section">
 <h2 class="section-title">Notifications</h2>
 <p class="login-copy">Send signed events when visitors save data. Owner writes and network-open sites do not send events.</p>
 <label>Site <select id="notify-site"><option value="">Choose a site</option></select></label>
 <p id="notify-status" role="status"></p>
 <div id="notify-list"></div>
 <form id="notify-form" hidden>
  <h3>Add notification</h3>
  <label>Label <input name="label" maxlength="200"></label>
  <label>Receiver URL <input name="url" type="url" placeholder="https://receiver.example.com/events" required></label>
  <label>Storage resource <input name="storage" placeholder="All resources"></label>
  <fieldset><legend>Events</legend>
   <label><input type="checkbox" name="events" value="record.added" checked> Record added</label>
   <label><input type="checkbox" name="events" value="record.changed"> Record edited or deleted</label>
   <label><input type="checkbox" name="events" value="file.uploaded"> File uploaded</label>
   <label><input type="checkbox" name="events" value="file.deleted"> File deleted</label>
  </fieldset>
  <label>Optional header name <input name="header_name" placeholder="Authorization"></label>
  <label>Header secret <input name="header_value" type="password" autocomplete="new-password"></label>
  <button type="submit" class="btn-login">Add notification</button>
 </form>
 <div id="notify-secret-box" hidden><p>Copy this signing secret to your receiver. It is shown only now.</p><pre id="notify-secret"></pre><button type="button" id="notify-dismiss">Dismiss secret</button></div>
 <pre id="notify-history" aria-live="polite" style="white-space:pre-wrap;overflow-wrap:anywhere"></pre>
</section>
<script>
(function(){
 var select=document.getElementById('notify-site'), form=document.getElementById('notify-form'), list=document.getElementById('notify-list'), status=document.getElementById('notify-status'), history=document.getElementById('notify-history');
 var headers={'Content-Type':'application/json','X-Simple-Host-Client':'control-ui'}, generation=0;
 async function api(path,method,body){
  var r=await fetch(path,{method:method||'GET',credentials:'same-origin',headers:headers,body:body===undefined?undefined:JSON.stringify(body)});
  var d=await r.json();if(!r.ok)throw new Error(d.error||'Request failed');return d;
 }
 function base(){return select.value;}
 function message(e){status.textContent=e.message||String(e);}
 function secret(value,until){document.getElementById('notify-secret-box').hidden=false;document.getElementById('notify-secret').textContent=value+(until?'\nPrevious secret valid until '+until:'');}
 document.getElementById('notify-dismiss').onclick=function(){document.getElementById('notify-secret').textContent='';document.getElementById('notify-secret-box').hidden=true;};
 function button(row,label,action){var b=document.createElement('button');b.type='button';b.className='btn-reject';b.textContent=label;b.onclick=async function(){b.disabled=true;try{await action();}catch(e){message(e);}finally{b.disabled=false;}};row.appendChild(b);}
 async function load(){
  var version=++generation,path=base();list.replaceChildren();history.textContent='';form.hidden=!path;if(!path)return;
  try{
   var d=await api(path);if(version!==generation)return;
   status.textContent=d.notifications.length+' subscriptions. Limit: '+d.limits.per_site+' per site; '+d.limits.history_days+' days of history.';
   d.notifications.forEach(function(n){
    var row=document.createElement('div');row.className='site-subsection';var title=document.createElement('p');title.textContent=(n.label||n.app_name||n.id)+' · '+n.state+' · '+n.events.join(', ');row.appendChild(title);
    var target=document.createElement('p');target.textContent=n.url;row.appendChild(target);
    var endpoint=path+'/'+encodeURIComponent(n.id);
    button(row,'Deliveries',async function(){var d=await api(endpoint+'/deliveries');history.textContent=d.deliveries.length?d.deliveries.map(function(x){return x.created_at+' '+x.name+' '+x.state+' ('+x.attempts+' attempts, '+(x.result||'pending')+')';}).join('\n'):'No deliveries yet.';});
    button(row,n.state==='active'?'Pause':'Resume',async function(){await api(endpoint,'PATCH',{paused:n.state==='active'});await load();});
    if(n.source==='owner'){
     button(row,'Send test',async function(){var d=await api(endpoint+'/test','POST');status.textContent=d.delivered?'Test delivered.':'Test failed: '+d.result;});
     button(row,'Rotate secret',async function(){var d=await api(endpoint+'/rotate-secret','POST');secret(d.secret,d.previous_valid_until);});
    }
    button(row,'Remove',async function(){if(!confirm('Remove this notification and its delivery history?'))return;await api(endpoint,'DELETE');await load();});
    list.appendChild(row);
   });
  }catch(e){message(e);}
 }
 select.onchange=load;
 form.onsubmit=async function(e){e.preventDefault();var data=new FormData(form),events=data.getAll('events');if(!events.length){message('Choose at least one event.');return;}
  var body={url:data.get('url'),label:data.get('label'),storage:data.get('storage'),events:events},header=data.get('header_name');
  if(header||data.get('header_value'))body.auth_header={name:header,value:data.get('header_value')};
  var submit=form.querySelector('button[type=submit]');submit.disabled=true;
  try{var d=await api(base(),'POST',body);form.reset();secret(d.secret);await load();}catch(e){message(e);}finally{submit.disabled=false;}
 };
 api('/api/collaboration/sites').then(function(data){var sites=Array.isArray(data)?data:(data.sites||[]);sites.forEach(function(s){if(s.access_role!=='owner'&&s.access_role!=='member')return;var o=document.createElement('option');o.value='/api/collaboration/sites/'+encodeURIComponent(s.owner_username)+'/'+encodeURIComponent(s.name)+'/notifications';o.textContent=s.owner_username+'/'+s.name;select.appendChild(o);});}).catch(message);
})();
</script>`

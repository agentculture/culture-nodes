"""Reverse transport shared by every bridge package."""
from __future__ import annotations
import json, logging, os, threading, time, urllib.error, urllib.request
LOG = logging.getLogger(__name__)
def configured(prefix):
    values=(os.environ.get(prefix+"_CONTROL_PLANE_URL","").rstrip("/"),os.environ.get(prefix+"_ACTOR_KEY",""),os.environ.get(prefix+"_DIAL_TOKEN",""))
    if not any(values): return None
    if not all(values): raise ValueError(prefix+"_CONTROL_PLANE_URL, _ACTOR_KEY and _DIAL_TOKEN must be set together")
    return values
def run(prefix, port, opener=urllib.request.urlopen, pause=time.sleep):
    base,actor,token=configured(prefix); headers={"Authorization":"Bearer "+token,"X-Culture-Nodes-Actor-Key":actor}
    while True:
        try:
            req=urllib.request.Request(base+"/v1alpha1/inbound/poll",data=b"",headers=headers,method="POST")
            try: response=opener(req,timeout=35)
            except urllib.error.HTTPError as exc:
                if exc.code==204: pause(.25); continue
                raise
            item=json.loads(response.read()); body=json.dumps(item["request"]).encode()
            local=urllib.request.Request(f"http://127.0.0.1:{port}/v1/invocations",data=body,headers={"Content-Type":"application/json","Idempotency-Key":item["attempt_id"]},method="POST")
            try: result=opener(local,timeout=65); status,payload=result.status,result.read()
            except urllib.error.HTTPError as exc: status,payload=exc.code,exc.read()
            complete=json.dumps({"status":status,"body":json.loads(payload)}).encode()
            done=urllib.request.Request(base+f"/v1alpha1/inbound/{item['id']}/complete",data=complete,headers={**headers,"Content-Type":"application/json"},method="POST")
            opener(done,timeout=15).close()
        except Exception as exc: LOG.warning("dial-in reconnecting: %s",exc); pause(1)
def start(prefix,port):
    if configured(prefix) is None: return None
    thread=threading.Thread(target=run,args=(prefix,port),daemon=True,name="dial-in"); thread.start(); return thread

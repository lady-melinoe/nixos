New Plan

 - Replicate subset of current configs on void host, using netns, and making all of the routes + wireguard + gre etc statically

 - use ucc vm's and *.testnet.melinoe.xyz for hosts

 - if it works, figure out how TF to port it to nixos

 - maybe just rewrite all configs, deploy to benzaiten and lachesis after resetting them, then test there. if it works there, then migrate dns & NPM & vaultwarden over, and then everything else
   and then migrate everything else over to it. 

 - this will all be a royal PITA.

 - alternatives: split horizon dns, passing public IPs around and using scripts to add rules to DNAT on the fly, swapping everything to l2 adjacency which would allow just setting gateway... 

 - actually l2 sounds really nice. although i'd have to figure out how to restrict permissions.... 
   - but l2 would give same issues. fuck.



 - maybe just re-architect all infra???? move to only phaesyle and ceridwen for now, reset all hosts, and design new infra from scratch???

 - advantages of moving to gretap and bgp evpn: lets me do cool gateway choosing bs
 - disadvantages: requires a pita security setup

 - but also l3 bullshit is nice
  - so probably just do that.
  - god fucking damn it this is annoying xD
  - 

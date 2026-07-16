#!/bin/bash
###############################################################################################################
# Allows mounting letsencrypt generate ssl certificates inside nginx container by granting your current user 
# acl read access to your .env specified letsencrypt live+archive directory
# there are better ways to do this....
# use at your own risk. 
###############################################################################################################
source .env
setfacl -R -m u:$(whoami):rx /etc/letsencrypt/live/${HOSTNAME} /etc/letsencrypt/archive/${HOSTNAME} /etc/letsencrypt/ssl-dhparams.pem /etc/letsencrypt/options-ssl-nginx.conf
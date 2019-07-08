#!/bin/bash

i=0
let timer=$1/$3

while [ $i -ne $timer ]
do
        (time wget -r $2) 2>> index.html
        sleep $3s
        i=$(($i+1))
done

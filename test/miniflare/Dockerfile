# node:26-slim; workerd needs glibc, so not alpine.
FROM node:26-slim@sha256:ec7758ee051e457b468b32bde57b0879010b325bb9862718e9615225ce4aaae1

WORKDIR /srv
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
COPY server.mjs ./

USER node
ENV MF_HOST=0.0.0.0 MF_PORT=8787
EXPOSE 8787
CMD ["node", "server.mjs"]

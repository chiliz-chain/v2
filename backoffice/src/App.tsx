import React, {FC} from 'react';
import {Button, Col, Input, List, Row, Tabs} from 'antd';
import './App.css';
import {observer} from "mobx-react";
import DeployerStore from "./stores/DeployerStore";

interface IAppProps {
  deployerStore: DeployerStore
}

const App: FC<IAppProps> = ({deployerStore}) => {
  return (
    (
      <div className="App">
        <br/>
        <br/>
        <Row>
          <Col xs={8} offset={8}>
            <Tabs defaultActiveKey={"1"}>
              <Tabs.TabPane tab={"Manage Deployers"} key={"1"}>
                <h4>List of deployers</h4>
                <List>
                  {deployerStore.deployers.map(deployer => {
                    return (
                      <List.Item>
                        {deployer}&nbsp;&nbsp;
                        <Button loading={deployerStore.isRemoving} onClick={() => {
                          // noinspection JSIgnoredPromiseFromCall
                          deployerStore.removeDeployer(deployer);
                        }} size={"small"}>Remove</Button>
                        {/*&nbsp;*/}
                        {/*<Button disabled={deployerStore.bannedDeployers.has(deployer)} loading={deployerStore.isBanning} onClick={() => {*/}
                        {/*  // noinspection JSIgnoredPromiseFromCall*/}
                        {/*  deployerStore.banDeployer(deployer);*/}
                        {/*}} size={"small"}>Ban</Button>*/}
                      </List.Item>
                    );
                  })}
                </List>
                <br/>
                <h4>Add new deployer</h4>
                <Input type={"text"} onChange={value => {
                  deployerStore.setNewDeployer(value.target.value);
                }} defaultValue={deployerStore.newDeployer}/>
                <br/>
                <br/>
                <Button loading={deployerStore.isAdding} type={"default"} onClick={() => {
                  // noinspection JSIgnoredPromiseFromCall
                  deployerStore.addDeployer(deployerStore.newDeployer);
                }}>Add Deployer</Button>
                &nbsp;
                <Button loading={deployerStore.isLoading} type={"default"} onClick={() => {
                  // noinspection JSIgnoredPromiseFromCall
                  deployerStore.loadDeployers();
                }}>Refresh List</Button>
              </Tabs.TabPane>
              <Tabs.TabPane tab={"Smart Contracts"} key={"2"}>
                <h4>List of smart contracts</h4>
                <List>
                  {deployerStore.deployers.map(deployer => {
                    return (
                      <List.Item>
                        {deployer}&nbsp;&nbsp;
                        <Button loading={deployerStore.isRemoving} onClick={() => {
                          // noinspection JSIgnoredPromiseFromCall
                          deployerStore.removeDeployer(deployer);
                        }} size={"small"}>Remove</Button>
                        {/*&nbsp;*/}
                        {/*<Button disabled={deployerStore.bannedDeployers.has(deployer)} loading={deployerStore.isBanning} onClick={() => {*/}
                        {/*  // noinspection JSIgnoredPromiseFromCall*/}
                        {/*  deployerStore.banDeployer(deployer);*/}
                        {/*}} size={"small"}>Ban</Button>*/}
                      </List.Item>
                    );
                  })}
                </List>
                <br/>
                <h4>Add new deployer</h4>
                <Input type={"text"} onChange={value => {
                  deployerStore.setNewDeployer(value.target.value);
                }} defaultValue={deployerStore.newDeployer}/>
                <br/>
                <br/>
                <Button loading={deployerStore.isAdding} type={"default"} onClick={() => {
                  // noinspection JSIgnoredPromiseFromCall
                  deployerStore.addDeployer(deployerStore.newDeployer);
                }}>Add Deployer</Button>
                &nbsp;
                <Button loading={deployerStore.isLoading} type={"default"} onClick={() => {
                  // noinspection JSIgnoredPromiseFromCall
                  deployerStore.loadDeployers();
                }}>Refresh List</Button>
              </Tabs.TabPane>
            </Tabs>
          </Col>
        </Row>
      </div>
    )
  );
};

export default observer(App);
